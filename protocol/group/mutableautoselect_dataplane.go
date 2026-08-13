package group

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/lantern-box/adapter"
)

// makeHooks returns the (onFailure, onActivity) callback pair wired into
// the data-plane wrappers. Callbacks re-look-up the member's history
// at fire time so a Remove+Add cycle can't strand writes on a stale
// entry. The failure callback short-circuits when recordUserFailure
// reports a dedup/non-member, so a single broken outbound with N
// orphan idle conns doesn't trigger N redundant full-fleet probe
// sweeps.
func (s *MutableAutoSelect) makeHooks(outerTag string) (onFailure func(adapter.UserFailureKind), onActivity func()) {
	onFailure = func(kind adapter.UserFailureKind) {
		if !s.recordUserFailure(outerTag, kind) {
			return
		}
		go s.runLadder(outerTag)
	}
	return onFailure, s.bumpActive
}

// dataPlaneWatchdog is the no-traffic stall timer shared by the stream
// and packet wrappers.
//
// provedReadBytes keeps handshake-only and keepalive-only conns from
// counting as stalls: the watchdog starts only after that many cumulative
// non-empty Read bytes have arrived.
//
// The write-vs-read classification distinguishes "tunnel is broken" from
// "user stopped sending traffic" on an already-proven conn. The stall
// fires only when the most recent non-empty IO was a Write — i.e. we sent
// bytes and got nothing back for the idle window. A proven conn whose
// last activity was a Read (response arrived, then silence) is treated
// as user-idle, not broken: a healthy keep-alive going unused looks
// identical to a broken tunnel without this gate.
//
// The classification and timestamp are packed into one atomic word so
// fireStall cannot combine a fresh write flag with a stale timestamp and
// falsely stall a just-written conn.
//
// The timer is lazy on two axes to keep its cost off the data path.
// It is allocated only when the conn becomes proven. noteIO also avoids
// timer resets on the IO path; it only stamps activity, and fireStall
// re-arms when the stamp is still fresh. Steady traffic therefore costs
// one timer op per idle window instead of one per packet.
type dataPlaneWatchdog struct {
	idle            time.Duration
	onFailure       func(adapter.UserFailureKind)
	onActivity      func()
	provedReadBytes uint64
	readBytes       atomic.Uint64
	proven          atomic.Bool
	// activity packs the last non-empty IO: bit 0 is the write flag, the
	// remaining bits are monotonic nanoseconds since dataPlaneEpoch.
	activity atomic.Int64
	stalled  atomic.Bool
	fired    atomic.Bool

	timerMu   sync.Mutex
	timer     *time.Timer // nil until proven; guarded by timerMu
	closeOnce sync.Once
}

// packActivity stores wasWrite in bit 0. This drops 1 ns of timestamp
// precision, which is irrelevant for idle-window comparisons.
func packActivity(nanos int64, wasWrite bool) int64 {
	v := nanos &^ 1
	if wasWrite {
		v |= 1
	}
	return v
}

func unpackActivity(v int64) (nanos int64, wasWrite bool) {
	return v &^ 1, v&1 != 0
}

// dataPlaneEpoch anchors activity timestamps to a monotonic clock, so
// wall-clock changes cannot delay or accelerate stall detection.
var dataPlaneEpoch = time.Now()

func sinceEpoch() int64 { return int64(time.Since(dataPlaneEpoch)) }

// idleForever seeds activity as old read-side IO: old enough to exceed
// any real idle window, but small enough to avoid subtraction overflow.
const idleForever = -int64(24 * time.Hour)

func (w *dataPlaneWatchdog) init(idle time.Duration, provedReadBytes uint64, onFailure func(adapter.UserFailureKind), onActivity func()) {
	w.idle = idle
	w.provedReadBytes = provedReadBytes
	w.onFailure = onFailure
	w.onActivity = onActivity
	// Direct fireStall calls in tests should evaluate the gates instead
	// of treating the missing IO stamp as fresh activity.
	w.activity.Store(idleForever)
	// Timer is armed lazily on the proven transition; see armTimer.
}

// armTimer starts the stall timer on the transition to proven. It skips
// closed conns so a late proven-crossing cannot leave a live timer.
func (w *dataPlaneWatchdog) armTimer() {
	w.timerMu.Lock()
	defer w.timerMu.Unlock()
	if w.stalled.Load() {
		return
	}
	w.timer = time.AfterFunc(w.idle, w.fireStall)
}

// rearm reschedules the stall timer for d, unless the conn is closed.
func (w *dataPlaneWatchdog) rearm(d time.Duration) {
	w.timerMu.Lock()
	defer w.timerMu.Unlock()
	if w.stalled.Load() || w.timer == nil {
		return
	}
	w.timer.Reset(d)
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

func (w *dataPlaneWatchdog) noteIO(n int, err error, isRead bool) {
	// Short-circuit once stalled: a late noteIO must not re-arm the
	// timer, fire onActivity, or attribute a failure after the conn is
	// logically gone.
	if w.stalled.Load() {
		return
	}
	// Attribute mid-stream transport failures immediately.
	if isDataPlaneFailure(err) {
		w.fireResetFailure()
		return
	}
	// Ignore empty I/O and benign terminal conditions.
	if n <= 0 || err != nil {
		return
	}
	// Stamp timestamp and write flag together so fireStall can't read a
	// fresh flag against a stale timestamp and fire on a just-written conn.
	w.activity.Store(packActivity(sinceEpoch(), !isRead))
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
		// Arm on the proven transition only: CAS so concurrent readers
		// crossing the threshold together arm exactly one timer.
		if w.proven.CompareAndSwap(false, true) {
			w.armTimer()
		}
	}
}

// closeWatchdog sets stalled before Stop so concurrent noteIO/fireStall
// cannot report a failure after Close. The timer is nil until proven.
func (w *dataPlaneWatchdog) closeWatchdog() (firstClose bool) {
	w.closeOnce.Do(func() {
		w.stalled.Store(true)
		w.timerMu.Lock()
		if w.timer != nil {
			w.timer.Stop()
		}
		w.timerMu.Unlock()
		firstClose = true
	})
	return firstClose
}

// fireStall is the idle-timer callback: it demotes a conn that went quiet
// mid-stream. It fires only on a proven conn whose last non-empty IO was a
// Write and whose idle window has genuinely elapsed.
//
// Since noteIO does not reset the timer, a callback can arrive while
// activity is fresh; fireStall re-arms for the remaining window. It also
// re-arms after read-only idle so later unanswered Writes remain watched.
func (w *dataPlaneWatchdog) fireStall() {
	if w.stalled.Load() {
		return
	}
	if !w.proven.Load() {
		return
	}
	lastNanos, lastWasWrite := unpackActivity(w.activity.Load())
	elapsed := time.Duration(sinceEpoch() - lastNanos)
	if remaining := w.idle - elapsed; remaining > 0 {
		w.rearm(remaining)
		return
	}
	if !lastWasWrite {
		w.rearm(w.idle)
		return
	}
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.onFailure != nil {
		w.onFailure(adapter.UserFailureStall)
	}
}

// fireResetFailure demotes on a mid-stream transport error. Unlike fireStall it skips
// the proven/lastWasWrite gates — an explicit reset is unambiguous even before a
// conn is proven. Fire and attribution happens at most once.
func (w *dataPlaneWatchdog) fireResetFailure() {
	if !w.stalled.CompareAndSwap(false, true) {
		return
	}
	if !w.fired.CompareAndSwap(false, true) {
		return
	}
	if w.onFailure != nil {
		// run onFailure in a goroutine to avoid deadlocks: the callback may call back
		// into the selector, which may hold locks that the data-plane IO path also needs.
		go w.onFailure(adapter.UserFailureReset)
	}
}

// dataPlaneStream detects tunnels that handshake successfully but
// stop carrying data after they have started carrying it.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, provedReadBytes uint64, onFailure func(adapter.UserFailureKind), onActivity func()) *dataPlaneStream {
	d := &dataPlaneStream{Conn: c}
	d.init(idle, provedReadBytes, onFailure, onActivity)
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

func newDataPlanePacket(
	c net.PacketConn,
	idle time.Duration,
	provedReadBytes uint64,
	onFailure func(adapter.UserFailureKind),
	onActivity func(),
) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c}
	d.init(idle, provedReadBytes, onFailure, onActivity)
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
