package group

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/adapter"
)

// Five seconds is a provisional margin below the default 10s DNS/NTP/STUN idle
// timeout. Delayed resets after partial replies can also be excused.
const defaultDataPlaneResetQuiet = 5 * time.Second

type dataPlaneHooks struct {
	onFailure func(adapter.UserFailureKind)
	// onError runs on the IO goroutine, outside the watchdog's lock.
	onError    func(err error, state dataPlaneIO, excused bool)
	onActivity func()
}

// makeHooks returns callbacks wired into data-plane wrappers. Failure
// handling re-checks membership at fire time, drops non-chargeable or
// deduped events, and starts a ladder only for recorded failures.
func (s *MutableAutoSelect) makeHooks(outerTag string, route routeKind) dataPlaneHooks {
	return dataPlaneHooks{
		onFailure: func(kind adapter.UserFailureKind) {
			if !s.chargeable(outerTag, route) {
				return
			}
			if !s.recordUserFailure(outerTag, kind) {
				return
			}
			go s.runLadder(outerTag)
		},
		onError: func(err error, state dataPlaneIO, excused bool) {
			decision := "candidate"
			if excused {
				decision = "excused"
			}
			last := "none"
			if state.hasIO {
				last = "read"
				if state.lastWasWrite {
					last = "write"
				}
			}
			s.logger.Debug("data-plane error: tag=", outerTag, " decision=", decision,
				" err=", err, " quiet=", state.quiet, " last=", last, " proven=", state.proven)
		},
		onActivity: s.bumpActive,
	}
}

type dataPlaneIO struct {
	hasIO        bool
	lastWasWrite bool
	quiet        time.Duration
	proven       bool
}

// dataPlaneWatchdog is the no-traffic stall timer shared by the stream
// and packet wrappers.
//
// provedReadBytes gates whether the stall is real: until the wrapped conn
// has delivered that many cumulative non-empty Read bytes, an
// idle-window expiry is treated as "established but never carried real
// traffic" (e.g. a handshake-only or keepalive-only conn) and the
// stall handler is suppressed.
//
// The direction of the last non-empty IO separately distinguishes "tunnel
// is broken" from "user stopped sending traffic" on an already-proven conn.
// The stall fires only when the most recent non-empty IO was a Write —
// i.e. we sent bytes and got nothing back for the idle window. A proven
// conn whose last activity was a Read (response arrived, then silence) is
// treated as user-idle, not broken: a healthy keep-alive going unused
// looks identical to a broken tunnel without this gate.
//
// endsQuietRead extends that rule to transport errors.
type dataPlaneWatchdog struct {
	idle            time.Duration
	hooks           dataPlaneHooks
	provedReadBytes uint64
	born            time.Time
	ioMu            sync.Mutex
	lastIO          time.Time // guarded by ioMu
	lastWasWrite    bool      // guarded by ioMu
	readBytes       atomic.Uint64
	proven          atomic.Bool
	stalled         atomic.Bool
	fired           atomic.Bool
	timer           *time.Timer
	closeOnce       sync.Once
}

func (w *dataPlaneWatchdog) init(idle time.Duration, provedReadBytes uint64, hooks dataPlaneHooks) {
	w.idle = idle
	w.provedReadBytes = provedReadBytes
	w.hooks = hooks
	w.born = time.Now()
	w.timer = time.AfterFunc(idle, w.fireStall)
}

// isDataPlaneFailure reports whether err should demote the outbound.
// Clean closes, caller cancellation, and timeouts are ignored.
func isDataPlaneFailure(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return true
}

func endsQuietRead(isRead bool, n int, state dataPlaneIO) bool {
	return isRead && n == 0 && state.hasIO && !state.lastWasWrite &&
		state.quiet >= defaultDataPlaneResetQuiet
}

func (w *dataPlaneWatchdog) noteIO(n int, err error, isRead bool) {
	// Short-circuit once stalled: a late noteIO must not re-arm the
	// timer, fire onActivity, or attribute a failure after the conn is
	// logically gone.
	if w.stalled.Load() {
		return
	}
	if isDataPlaneFailure(err) {
		state := w.snapshotIO()
		excused := endsQuietRead(isRead, n, state)
		if !excused {
			w.fireResetFailure()
		}
		if w.hooks.onError != nil {
			w.hooks.onError(err, state, excused)
		}
		return
	}
	// Ignore empty I/O and benign terminal conditions.
	if n <= 0 || err != nil {
		return
	}
	// Publish the IO state before re-arming the stall timer.
	w.ioMu.Lock()
	w.lastIO = time.Now()
	w.lastWasWrite = !isRead
	w.ioMu.Unlock()
	w.timer.Reset(w.idle)
	if w.hooks.onActivity != nil {
		w.hooks.onActivity()
	}
	if !isRead {
		return
	}
	// Mark the conn proven once cumulative Read bytes cross the
	// threshold. provedReadBytes==0 keeps the legacy "any non-empty Read
	// proves" behavior for tests that don't care about the threshold;
	// production callers always pass a non-zero default.
	if w.proven.Load() {
		return
	}
	total := w.readBytes.Add(uint64(n))
	if total >= w.provedReadBytes {
		w.proven.Store(true)
	}
}

func (w *dataPlaneWatchdog) snapshotIO() dataPlaneIO {
	w.ioMu.Lock()
	defer w.ioMu.Unlock()
	last := w.lastIO
	hasIO := !last.IsZero()
	if !hasIO {
		last = w.born
	}
	return dataPlaneIO{
		hasIO:        hasIO,
		lastWasWrite: w.lastWasWrite,
		quiet:        time.Since(last),
		proven:       w.proven.Load(),
	}
}

// closeWatchdog sets stalled=true before Stop so any concurrent noteIO
// short-circuits and any concurrent fireStall CAS-fails — late I/O can't
// deliver a phantom onFailure after Close.
func (w *dataPlaneWatchdog) closeWatchdog() (firstClose bool) {
	w.closeOnce.Do(func() {
		w.stalled.Store(true)
		w.timer.Stop()
		firstClose = true
	})
	return firstClose
}

// fireStall is the idle-timer callback: it demotes a conn that went quiet
// mid-stream. It fires only on a proven conn whose last non-empty IO was a
// Write.
func (w *dataPlaneWatchdog) fireStall() {
	if !w.proven.Load() {
		return
	}
	if !w.snapshotIO().lastWasWrite {
		return
	}
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onFailure != nil {
		w.hooks.onFailure(adapter.UserFailureStall)
	}
}

// fireResetFailure attributes a non-excused error once, even on an unproven conn.
func (w *dataPlaneWatchdog) fireResetFailure() {
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.hooks.onFailure != nil {
		// run onFailure in a goroutine to avoid deadlocks: the callback may call back
		// into the selector, which may hold locks that the data-plane IO path also needs.
		go w.hooks.onFailure(adapter.UserFailureReset)
	}
}

// dataPlaneStream detects tunnels that handshake successfully but
// stop carrying data after they have started carrying it.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, provedReadBytes uint64, hooks dataPlaneHooks) *dataPlaneStream {
	d := &dataPlaneStream{Conn: c}
	d.init(idle, provedReadBytes, hooks)
	return d
}

func (d *dataPlaneStream) Read(p []byte) (int, error) {
	n, err := d.Conn.Read(p)
	d.noteIO(n, err, true)
	return n, err
}

func (d *dataPlaneStream) Write(p []byte) (int, error) {
	n, err := d.Conn.Write(p)
	d.noteIO(n, err, false)
	return n, err
}

// Close is idempotent: sing-box's connection lifecycle frequently
// double-closes, so the underlying Close runs at most once.
func (d *dataPlaneStream) Close() error {
	if !d.closeWatchdog() {
		return nil
	}
	return d.Conn.Close()
}

// Upstream lets common.Cast descend through the wrapper to find feature
// interfaces (EarlyConn, VectorisedConn, ReadWaiter, ...) on the inner
// outbound; without this, fast paths downstream silently disable.
func (d *dataPlaneStream) Upstream() any { return d.Conn }

// dataPlanePacket wraps a net.PacketConn; same contract as dataPlaneStream.
type dataPlanePacket struct {
	net.PacketConn
	dataPlaneWatchdog
	writer N.PacketWriter
}

func newDataPlanePacket(
	c net.PacketConn,
	idle time.Duration,
	provedReadBytes uint64,
	hooks dataPlaneHooks,
) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c, writer: bufio.NewPacketConn(c)}
	d.init(idle, provedReadBytes, hooks)
	return d
}

func (d *dataPlanePacket) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := d.PacketConn.ReadFrom(p)
	d.noteIO(n, err, true)
	return n, addr, err
}

func (d *dataPlanePacket) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := d.PacketConn.WriteTo(p, addr)
	d.noteIO(n, err, false)
	return n, err
}

// ReadPacket reads through ReadFrom, so the watchdog sees the read.
func (d *dataPlanePacket) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(d)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

// WritePacket passes domain-name destinations through; bufio's
// net.PacketConn adapter would strip them to IP-only.
func (d *dataPlanePacket) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	n := buffer.Len()
	err := d.writer.WritePacket(buffer, destination)
	d.noteIO(n, err, false)
	return err
}

func (d *dataPlanePacket) Close() error {
	if !d.closeWatchdog() {
		return nil
	}
	return d.PacketConn.Close()
}

func (d *dataPlanePacket) Upstream() any { return d.PacketConn }

var (
	_ net.Conn        = (*dataPlaneStream)(nil)
	_ N.NetPacketConn = (*dataPlanePacket)(nil)
)
