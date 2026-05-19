package group

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// makeHooks returns the (onStall, onRead) callback pair wired into the
// data-plane wrappers. Callbacks re-look-up the member's history at fire
// time so a Remove+Add cycle can't strand writes on a stale entry.
func (s *MutableAutoSelect) makeHooks(outerTag string) (onStall, onRead func()) {
	onStall = func() {
		s.recordUserFailure(outerTag)
		go s.runLadder(outerTag)
	}
	onRead = func() {
		// Peek (not historyForLocked): a tag whose first event is a
		// successful Read has no user_failures to reset, and creating
		// an empty entry just litters the map.
		s.access.Lock()
		defer s.access.Unlock()
		if _, member := s.members.Load(outerTag); !member {
			return
		}
		h, ok := s.peekHistoryLocked(outerTag)
		if !ok || !h.resetUserFailures() || s.history == nil {
			return
		}
		s.history.Store(outerTag, h.toTagHistory(time.Now()))
	}
	return onStall, onRead
}

// dataPlaneWatchdog drives the no-traffic stall timer and the three
// optional activity callbacks shared by the stream and packet wrappers.
//
//   - onStall fires once if the idle threshold elapses with no I/O.
//   - onActivity fires on every non-empty Read or Write (Write-as-liveness
//     is good enough for the adaptive probe-cadence signal).
//   - onRead fires only on non-empty Read; the user-failure reset needs
//     unambiguous bidirectional evidence because a QUIC/HTTP2 Write only
//     proves the local stack accepted the bytes, not that the peer did.
type dataPlaneWatchdog struct {
	idle       time.Duration
	onStall    func()
	onActivity func()
	onRead     func()
	stalled    atomic.Bool
	timer      *time.Timer
	closeOnce  sync.Once
}

func (w *dataPlaneWatchdog) init(idle time.Duration, onStall, onActivity, onRead func()) {
	w.idle = idle
	w.onStall = onStall
	w.onActivity = onActivity
	w.onRead = onRead
	w.timer = time.AfterFunc(idle, w.fireStall)
}

func (w *dataPlaneWatchdog) noteIO(n int, err error, isRead bool) {
	if n <= 0 || err != nil {
		return
	}
	w.timer.Reset(w.idle)
	if w.onActivity != nil {
		w.onActivity()
	}
	if isRead && w.onRead != nil {
		w.onRead()
	}
}

// closeWatchdog stops the timer and sets stalled=true so a Read/Write
// that races Close — returning n>0 just before Stop and then re-arming
// via Reset — can't deliver a phantom onStall after the conn is gone.
func (w *dataPlaneWatchdog) closeWatchdog() (firstClose bool) {
	w.closeOnce.Do(func() {
		w.stalled.Store(true)
		w.timer.Stop()
		firstClose = true
	})
	return firstClose
}

func (w *dataPlaneWatchdog) fireStall() {
	if w.stalled.CompareAndSwap(false, true) {
		w.onStall()
	}
}

// dataPlaneStream wraps a net.Conn with the stall watchdog and activity
// hooks. Detects tunnels that handshake successfully but never carry data.
type dataPlaneStream struct {
	net.Conn
	dataPlaneWatchdog
}

func newDataPlaneStream(c net.Conn, idle time.Duration, onStall, onActivity, onRead func()) *dataPlaneStream {
	d := &dataPlaneStream{Conn: c}
	d.init(idle, onStall, onActivity, onRead)
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

func newDataPlanePacket(c net.PacketConn, idle time.Duration, onStall, onActivity, onRead func()) *dataPlanePacket {
	d := &dataPlanePacket{PacketConn: c}
	d.init(idle, onStall, onActivity, onRead)
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
