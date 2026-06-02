package group

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// makeHooks returns the (onStall, onActivity) callback pair wired into
// the data-plane wrappers. Callbacks re-look-up the member's history
// at fire time so a Remove+Add cycle can't strand writes on a stale
// entry. The stall callback short-circuits when recordUserFailure
// reports a dedup/non-member, so a single broken outbound with N
// orphan idle conns doesn't trigger N redundant full-fleet probe
// sweeps.
func (s *MutableAutoSelect) makeHooks(outerTag string) (onStall, onActivity func()) {
	onStall = func() {
		if !s.recordUserFailure(outerTag) {
			return
		}
		go s.runLadder(outerTag)
	}
	return onStall, s.bumpActive
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
// lastWasWrite separately distinguishes "tunnel is broken" from "user
// stopped sending traffic" on an already-proven conn. The stall fires
// only when the most recent non-empty IO was a Write — i.e. we sent
// bytes and got nothing back for the idle window. A proven conn whose
// last activity was a Read (response arrived, then silence) is treated
// as user-idle, not broken: a healthy keep-alive going unused looks
// identical to a broken tunnel without this gate.
type dataPlaneWatchdog struct {
	idle            time.Duration
	onStall         func()
	onActivity      func()
	provedReadBytes uint64
	readBytes       atomic.Uint64
	proven          atomic.Bool
	lastWasWrite    atomic.Bool
	stalled         atomic.Bool
	fired           atomic.Bool
	timer           *time.Timer
	closeOnce       sync.Once
}

func (w *dataPlaneWatchdog) init(idle time.Duration, provedReadBytes uint64, onStall, onActivity func()) {
	w.idle = idle
	w.provedReadBytes = provedReadBytes
	w.onStall = onStall
	w.onActivity = onActivity
	w.timer = time.AfterFunc(idle, w.fireStall)
}

func (w *dataPlaneWatchdog) noteIO(n int, err error, isRead bool) {
	if n <= 0 || err != nil {
		return
	}
	// Short-circuit once stalled: a late noteIO must not re-arm the
	// timer or fire onActivity after the conn is logically gone.
	if w.stalled.Load() {
		return
	}
	// Publish the gate value before re-arming the timer so a fireStall
	// racing the Reset reads the fresh classification, not the previous
	// IO's value. Otherwise a Read landing just as the timer fires
	// could leave lastWasWrite=true from an earlier Write and trip
	// the gate it's supposed to suppress.
	w.lastWasWrite.Store(!isRead)
	w.timer.Reset(w.idle)
	if w.onActivity != nil {
		w.onActivity()
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

// closeWatchdog sets stalled=true before Stop so any concurrent noteIO
// short-circuits and any concurrent fireStall CAS-fails — late I/O can't
// deliver a phantom onStall after Close.
func (w *dataPlaneWatchdog) closeWatchdog() (firstClose bool) {
	w.closeOnce.Do(func() {
		w.stalled.Store(true)
		w.timer.Stop()
		firstClose = true
	})
	return firstClose
}

// fireStall is the AfterFunc callback. The stall counts only if the
// conn has delivered enough Read payload to be proven AND the most
// recent non-empty IO was a Write — i.e. we sent bytes and heard
// nothing back. A proven conn whose last activity was a Read is
// treated as user-idle and suppressed: an unused HTTPS keep-alive
// looks identical to a broken tunnel without this gate. Other failure
// paths (dial errors, probe failures) still catch the cases this gate
// suppresses. fired is the once-per-conn CAS so a re-armed timer
// can't double-deliver onStall.
func (w *dataPlaneWatchdog) fireStall() {
	if !w.proven.Load() {
		return
	}
	if !w.lastWasWrite.Load() {
		return
	}
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.onStall != nil {
		w.onStall()
	}
}

// dataPlaneStream detects tunnels that handshake successfully but
// stop carrying data after they have started carrying it.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, provedReadBytes uint64, onStall, onActivity func()) *dataPlaneStream {
	d := &dataPlaneStream{Conn: c}
	d.init(idle, provedReadBytes, onStall, onActivity)
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
}

func newDataPlanePacket(c net.PacketConn, idle time.Duration, provedReadBytes uint64, onStall, onActivity func()) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c}
	d.init(idle, provedReadBytes, onStall, onActivity)
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

func (d *dataPlanePacket) Close() error {
	if !d.closeWatchdog() {
		return nil
	}
	return d.PacketConn.Close()
}

func (d *dataPlanePacket) Upstream() any { return d.PacketConn }

var (
	_ net.Conn       = (*dataPlaneStream)(nil)
	_ net.PacketConn = (*dataPlanePacket)(nil)
)
