package unbounded

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"golang.org/x/time/rate"
)

// minBurst keeps WaitN satisfiable for the largest UDP datagram and smooths
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
	tx *rate.Limiter
	rx *rate.Limiter
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

func newLimiter(bytesPerSec byteRate) *rate.Limiter {
	burst := int(bytesPerSec) / burstFraction
	if burst < minBurst {
		burst = minBurst
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

func (n *limitedNet) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
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
type connLimits struct {
	tx *rate.Limiter
	rx *rate.Limiter

	closed context.Context
	close  context.CancelFunc

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func newConnLimits(tx, rx *rate.Limiter) *connLimits {
	ctx, cancel := context.WithCancel(context.Background())
	return &connLimits{tx: tx, rx: rx, closed: ctx, close: cancel}
}

func (l *connLimits) setDeadline(read, write bool, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if read {
		l.readDeadline = t
	}
	if write {
		l.writeDeadline = t
	}
}

// waitCtx is the context a limiter wait runs under: cancelled on close and
// bounded by the direction's deadline, if any.
func (l *connLimits) waitCtx(write bool) (context.Context, context.CancelFunc) {
	l.mu.Lock()
	deadline := l.readDeadline
	if write {
		deadline = l.writeDeadline
	}
	l.mu.Unlock()
	if deadline.IsZero() {
		return context.WithCancel(l.closed)
	}
	return context.WithDeadline(l.closed, deadline)
}

// waitTx blocks until n bytes may be sent. A datagram larger than the burst
// can never be admitted, so it is sent without waiting rather than erroring.
// An interrupted wait releases its reservation and reports why.
func (l *connLimits) waitTx(n int) error {
	if n > l.tx.Burst() {
		return nil
	}
	ctx, cancel := l.waitCtx(true)
	defer cancel()
	if err := l.tx.WaitN(ctx, n); err != nil {
		return waitErr(l.closed, err)
	}
	return nil
}

// tookRx charges n received bytes, blocking the reader if the budget is
// spent. The bytes are already in the caller's buffer, so an interrupted wait
// only stops the blocking; the read still succeeds.
func (l *connLimits) tookRx(n int) {
	if n <= 0 || n > l.rx.Burst() {
		return
	}
	ctx, cancel := l.waitCtx(false)
	defer cancel()
	_ = l.rx.WaitN(ctx, n)
}

// waitErr maps an interrupted limiter wait to the error the socket would
// have returned itself. Oversized requests never reach WaitN, so it fails
// only for a closed socket or a deadline, whether the deadline passed during
// the wait or the limiter saw up front that the wait would overrun it.
func waitErr(closed context.Context, err error) error {
	if closed.Err() != nil || errors.Is(err, context.Canceled) {
		return net.ErrClosed
	}
	return os.ErrDeadlineExceeded
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
	c.limits.setDeadline(true, true, t)
	return c.UDPConn.SetDeadline(t)
}

func (c *limitedUDPConn) SetReadDeadline(t time.Time) error {
	c.limits.setDeadline(true, false, t)
	return c.UDPConn.SetReadDeadline(t)
}

func (c *limitedUDPConn) SetWriteDeadline(t time.Time) error {
	c.limits.setDeadline(false, true, t)
	return c.UDPConn.SetWriteDeadline(t)
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
	return c.UDPConn.Write(b)
}

func (c *limitedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(b, addr)
}

func (c *limitedUDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteToUDP(b, addr)
}

func (c *limitedUDPConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if err := c.limits.waitTx(len(b)); err != nil {
		return 0, 0, err
	}
	return c.UDPConn.WriteMsgUDP(b, oob, addr)
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
	c.limits.setDeadline(true, true, t)
	return c.PacketConn.SetDeadline(t)
}

func (c *limitedPacketConn) SetReadDeadline(t time.Time) error {
	c.limits.setDeadline(true, false, t)
	return c.PacketConn.SetReadDeadline(t)
}

func (c *limitedPacketConn) SetWriteDeadline(t time.Time) error {
	c.limits.setDeadline(false, true, t)
	return c.PacketConn.SetWriteDeadline(t)
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
	return c.PacketConn.WriteTo(b, addr)
}
