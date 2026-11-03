package unbounded

import (
	"context"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
)

// minBurst keeps every UDP datagram within what the bucket can grant and smooths
// the limiter at low rates.
const minBurst = 64 * 1024

// burstFraction is the share of a second's budget a socket may send at once
// before the limiter paces it: 100 ms of traffic, so a short burst passes
// but a sustained one is shaped closely to the limit.
const burstFraction = 10

// Ported from getlantern/lantern-headless-client#12 by Ilya Yakelzon (reflog).

// limitedNet is the host's network stack with every UDP socket pion opens on
// it rate-limited. Outbound writes wait for tokens before sending, which
// shapes what this peer uploads toward consumers. Inbound reads take their
// tokens after the datagram arrives, so a flood fills the socket buffer and
// the kernel drops the excess; the peer's congestion control then slows down.
// One limiter per direction is shared by all sockets, so the limit is for the
// whole process, not per peer.
type limitedNet struct {
	*stdnet.Net
	tx *tokenBucket
	rx *tokenBucket
}

// newLimitedNet returns a transport.Net that caps UDP traffic at bytesPerSec
// in each direction.
func newLimitedNet(bytesPerSec byteRate) (transport.Net, error) {
	std, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &limitedNet{
		Net: std,
		tx:  newLimiter(bytesPerSec),
		rx:  newLimiter(bytesPerSec),
	}, nil
}

func newLimiter(bytesPerSec byteRate) *tokenBucket {
	burst := int(bytesPerSec) / burstFraction
	if burst < minBurst {
		burst = minBurst
	}
	return newTokenBucket(float64(bytesPerSec), burst)
}

// stdnet.Net captures the host's interfaces when it is created, and pion made
// a fresh one per peer connection. This Net lives as long as the volunteer, so
// it answers interface lookups from a fresh snapshot each time, or new
// connections would miss host candidates after a network change.
var newStdNet = stdnet.NewNet

func (n *limitedNet) Interfaces() ([]*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	return fresh.Interfaces()
}

func (n *limitedNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	return fresh.InterfaceByIndex(index)
}

func (n *limitedNet) InterfaceByName(name string) (*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	return fresh.InterfaceByName(name)
}

func (n *limitedNet) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	if laddr != nil && laddr.IP.IsMulticast() {
		// Only pion's mDNS listens on a multicast address, and x/net, which
		// it reads that socket through, accepts only a *net.UDPConn. Its
		// traffic is a few small packets, so it goes unlimited.
		return c, nil
	}
	return &limitedUDPConn{UDPConn: c, limits: newConnLimits(n.tx, n.rx)}, nil
}

func (n *limitedNet) ListenPacket(network, address string) (net.PacketConn, error) {
	c, err := n.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	if udp, ok := c.(transport.UDPConn); ok {
		return &limitedUDPConn{UDPConn: udp, limits: newConnLimits(n.tx, n.rx)}, nil
	}
	return &limitedPacketConn{PacketConn: c, limits: newConnLimits(n.tx, n.rx)}, nil
}

func (n *limitedNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.DialUDP(network, laddr, raddr)
	if err != nil {
		return nil, err
	}
	return &limitedUDPConn{UDPConn: c, limits: newConnLimits(n.tx, n.rx)}, nil
}

// connLimits is one socket's view of the shared limiters. A wait on a spent
// bucket ends when the socket is closed or its deadline passes, so a closed
// socket does not hold its goroutine and buffers for the rest of the wait.
// Like the socket's own I/O, a pending wait follows deadline changes: pion's
// UDP mux aborts a blocked write by setting the write deadline to now.
type connLimits struct {
	tx *tokenBucket
	rx *tokenBucket

	closed context.Context
	close  context.CancelFunc

	mu    sync.Mutex
	read  deadline
	write deadline

	// setMu serializes deadline updates across the limiter's copy and the
	// socket's, so concurrent setters can't leave the two disagreeing.
	setMu sync.Mutex
}

// deadline is one direction's deadline, and a channel closed when it changes.
type deadline struct {
	at      time.Time
	changed chan struct{}
}

func (d *deadline) set(t time.Time) {
	d.at = t
	close(d.changed)
	d.changed = make(chan struct{})
}

func newConnLimits(tx, rx *tokenBucket) *connLimits {
	ctx, cancel := context.WithCancel(context.Background())
	return &connLimits{
		tx: tx, rx: rx, closed: ctx, close: cancel,
		read:  deadline{changed: make(chan struct{})},
		write: deadline{changed: make(chan struct{})},
	}
}

// setDeadline sets the deadline for read, write or both, on the limiter's
// copy and, through set, on the socket, as one update.
func (l *connLimits) setDeadline(read, write bool, t time.Time, set func(time.Time) error) error {
	l.setMu.Lock()
	defer l.setMu.Unlock()
	l.mu.Lock()
	if read {
		l.read.set(t)
	}
	if write {
		l.write.set(t)
	}
	l.mu.Unlock()
	return set(t)
}

func (l *connLimits) deadlineOf(write bool) deadline {
	l.mu.Lock()
	defer l.mu.Unlock()
	if write {
		return l.write
	}
	return l.read
}

// unchanged reports whether the direction's deadline is still d, so a timeout
// read from d may be committed: a setter may have moved it since.
func (l *connLimits) unchanged(write bool, d deadline) bool {
	return l.deadlineOf(write).changed == d.changed
}

// afterDeadlineRead and afterEarlyDeadlineRead, if set, run in wait right
// after it reads the deadline, in its loop and in its check before reserving.
// Tests use them to move the deadline at that point.
var afterDeadlineRead, afterEarlyDeadlineRead func()

// wait takes n tokens from b, blocking until they are granted. It returns
// net.ErrClosed if the socket closes first, or os.ErrDeadlineExceeded if the
// direction's deadline, as it stands at any point during the wait, passes
// first; either way the wait is withdrawn and costs no tokens.
func (l *connLimits) wait(b *tokenBucket, n int, write bool) error {
	// A socket that's closed or past its deadline will fail the I/O anyway,
	// so it mustn't spend tokens other sockets share.
	if l.closed.Err() != nil {
		return net.ErrClosed
	}
	for {
		d := l.deadlineOf(write)
		if afterEarlyDeadlineRead != nil {
			afterEarlyDeadlineRead()
		}
		if d.at.IsZero() || time.Now().Before(d.at) {
			break
		}
		if l.unchanged(write, d) {
			return os.ErrDeadlineExceeded
		}
	}
	w := b.take(n)
	if w == nil {
		return nil
	}
	for {
		d := l.deadlineOf(write)
		if afterDeadlineRead != nil {
			afterDeadlineRead()
		}
		if !d.at.IsZero() && !time.Now().Before(d.at) {
			if !l.unchanged(write, d) {
				continue
			}
			b.cancel(w)
			return os.ErrDeadlineExceeded
		}
		granted, wait, changed := b.poll(w)
		if granted {
			return nil
		}
		wakeTimer := time.NewTimer(wait)
		var expired <-chan time.Time
		var deadlineTimer *time.Timer
		if !d.at.IsZero() {
			deadlineTimer = time.NewTimer(time.Until(d.at))
			expired = deadlineTimer.C
		}
		var err error
		done := true
		select {
		case <-w.ready:
		case <-wakeTimer.C:
			// Time to refill and grant again.
			done = false
		case <-changed:
			// Tokens came back or a waiter ahead left: recompute.
			done = false
		case <-l.closed.Done():
			b.cancel(w)
			err = net.ErrClosed
		case <-expired:
			if l.unchanged(write, d) {
				b.cancel(w)
				err = os.ErrDeadlineExceeded
			} else {
				done = false
			}
		case <-d.changed:
			done = false
		}
		wakeTimer.Stop()
		if deadlineTimer != nil {
			deadlineTimer.Stop()
		}
		if done {
			return err
		}
	}
}

// waitTx blocks until n bytes may be sent. A datagram larger than the burst
// can never be admitted, so it is sent without waiting rather than erroring.
// An interrupted wait costs no tokens and reports why.
func (l *connLimits) waitTx(n int) error {
	if n > l.tx.Burst() {
		return nil
	}
	return l.wait(l.tx, n, true)
}

// refundTx puts back the tokens waitTx took for n bytes whose write then
// failed, whether the socket rejected it or closed or timed out just after the
// grant.
func (l *connLimits) refundTx(n int) {
	if n > l.tx.Burst() {
		return
	}
	l.tx.refund(n)
}

// tookRx charges n received bytes, blocking the reader if the budget is
// spent. The bytes are already in the caller's buffer, so an interrupted wait
// only stops the blocking; the read still succeeds.
func (l *connLimits) tookRx(n int) {
	if n <= 0 || n > l.rx.Burst() {
		return
	}
	_ = l.wait(l.rx, n, false)
}

// limitedUDPConn applies the shared limiters to every read and write path of
// a transport.UDPConn. pion's ICE agent uses WriteTo and ReadFrom; the rest
// are covered so a different caller cannot bypass the limit.
type limitedUDPConn struct {
	transport.UDPConn
	limits *connLimits
}

func (c *limitedUDPConn) Close() error {
	c.limits.close()
	return c.UDPConn.Close()
}

func (c *limitedUDPConn) SetDeadline(t time.Time) error {
	return c.limits.setDeadline(true, true, t, c.UDPConn.SetDeadline)
}

func (c *limitedUDPConn) SetReadDeadline(t time.Time) error {
	return c.limits.setDeadline(true, false, t, c.UDPConn.SetReadDeadline)
}

func (c *limitedUDPConn) SetWriteDeadline(t time.Time) error {
	return c.limits.setDeadline(false, true, t, c.UDPConn.SetWriteDeadline)
}

func (c *limitedUDPConn) Read(b []byte) (int, error) {
	n, err := c.UDPConn.Read(b)
	c.limits.tookRx(n)
	return n, err
}

func (c *limitedUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.UDPConn.ReadFrom(b)
	c.limits.tookRx(n)
	return n, addr, err
}

func (c *limitedUDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, addr, err := c.UDPConn.ReadFromUDP(b)
	c.limits.tookRx(n)
	return n, addr, err
}

func (c *limitedUDPConn) ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error) {
	n, oobn, flags, addr, err := c.UDPConn.ReadMsgUDP(b, oob)
	c.limits.tookRx(n)
	return n, oobn, flags, addr, err
}

func (c *limitedUDPConn) Write(b []byte) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	n, err := c.UDPConn.Write(b)
	if err != nil {
		c.limits.refundTx(len(b))
	}
	return n, err
}

func (c *limitedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	n, err := c.UDPConn.WriteTo(b, addr)
	if err != nil {
		c.limits.refundTx(len(b))
	}
	return n, err
}

func (c *limitedUDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	n, err := c.UDPConn.WriteToUDP(b, addr)
	if err != nil {
		c.limits.refundTx(len(b))
	}
	return n, err
}

func (c *limitedUDPConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, 0, err
	}
	n, oobn, err := c.UDPConn.WriteMsgUDP(b, oob, addr)
	if err != nil {
		c.limits.refundTx(len(b))
	}
	return n, oobn, err
}

// limitedPacketConn is the plain net.PacketConn variant for sockets stdnet
// returns that are not UDP.
type limitedPacketConn struct {
	net.PacketConn
	limits *connLimits
}

func (c *limitedPacketConn) Close() error {
	c.limits.close()
	return c.PacketConn.Close()
}

func (c *limitedPacketConn) SetDeadline(t time.Time) error {
	return c.limits.setDeadline(true, true, t, c.PacketConn.SetDeadline)
}

func (c *limitedPacketConn) SetReadDeadline(t time.Time) error {
	return c.limits.setDeadline(true, false, t, c.PacketConn.SetReadDeadline)
}

func (c *limitedPacketConn) SetWriteDeadline(t time.Time) error {
	return c.limits.setDeadline(false, true, t, c.PacketConn.SetWriteDeadline)
}

func (c *limitedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	c.limits.tookRx(n)
	return n, addr, err
}

func (c *limitedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	n, err := c.PacketConn.WriteTo(b, addr)
	if err != nil {
		c.limits.refundTx(len(b))
	}
	return n, err
}
