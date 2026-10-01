package clientcontext

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

// exchangeTimeout bounds a frame write made before the dial returns, after the
// dial-layer timeouts no longer apply.
var exchangeTimeout = 10 * time.Second

var probePayload = []byte(clientInfoPrefix + "{}")

func (i *Injector) encodePayload(ctx context.Context) []byte {
	if lAdapter.IsProbe(ctx) {
		return probePayload
	}
	// ClientInfo has only string and bool fields, so Marshal cannot fail.
	buf, _ := json.Marshal(i.getInfo())
	return append([]byte(clientInfoPrefix), buf...)
}

// runExchange bounds exchange by ctx and exchangeTimeout. It closes conn on
// failure and leaves it open on success. If ctx ends or the timeout is reached
// first, it returns ctx's error.
func runExchange(ctx context.Context, conn io.Closer, exchange func() error) error {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		conn.Close()
		return err
	}

	// Closing is the only way to unblock I/O on conns that ignore deadlines.
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
		close(closed)
	})
	err := exchange()
	if !stop() {
		// Wait so conn is closed by the time the error is returned.
		<-closed
		return ctx.Err()
	}
	if err != nil {
		conn.Close()
	}
	return err
}

// exchangeStream writes the frame payload to conn without waiting for a reply.
// It closes conn on failure.
func exchangeStream(ctx context.Context, conn net.Conn, payload []byte) error {
	return runExchange(ctx, conn, func() error {
		_, err := conn.Write(payload)
		return err
	})
}

// exchangePacket writes the frame payload as one packet to destination without
// waiting for a reply. It closes conn on failure.
func exchangePacket(ctx context.Context, conn net.PacketConn, destination M.Socksaddr, payload []byte) error {
	// A domain destination is passed through as a Socksaddr so the outbound
	// can carry it to the server; UDPAddr would drop the name.
	var addr net.Addr = destination
	if destination.IsIP() {
		addr = destination.UDPAddr()
	}
	return runExchange(ctx, conn, func() error {
		_, err := conn.WriteTo(payload, addr)
		return err
	})
}

var (
	_ N.EarlyConn          = (*frameConn)(nil)
	_ N.EarlyWriter        = (*frameConn)(nil)
	_ N.ReaderWithUpstream = (*frameConn)(nil)
	_ N.WriterWithUpstream = (*frameConn)(nil)
)

// frameConn sends the frame payload together with the first write, so a
// postponed protocol handshake still carries the caller's first bytes.
type frameConn struct {
	net.Conn
	early   N.EarlyConn
	writeMu sync.Mutex
	frame   []byte // guarded by writeMu; nil once sent
	sent    atomic.Bool
}

func newFrameConn(conn net.Conn, early N.EarlyConn, payload []byte) *frameConn {
	return &frameConn{Conn: conn, early: early, frame: payload}
}

func (c *frameConn) Write(b []byte) (int, error) {
	if c.sent.Load() {
		return c.Conn.Write(b)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.sent.Load() {
		return c.Conn.Write(b)
	}
	frame := c.frame
	// One inner write, so the handshake carries the frame and b together. The
	// router's handshake kick writes nil, which sends the frame alone.
	// Clip so append never writes into a frame's spare capacity, which for
	// probes is the shared probePayload.
	n, err := c.Conn.Write(append(slices.Clip(frame), b...))
	if n < len(frame) {
		// Keep the unsent part for the next write: the router ignores a timed-out
		// handshake kick and keeps relaying.
		c.frame = frame[n:]
		return 0, err
	}
	c.frame = nil
	// Set only once the frame is out, so a writer on the unlocked path cannot
	// overtake it.
	c.sent.Store(true)
	return n - len(frame), err
}

func (c *frameConn) NeedHandshake() bool {
	return !c.sent.Load() || c.early.NeedHandshake()
}

// NeedHandshakeForWrite reports the same as NeedHandshake. The router checks
// for N.EarlyConn only on the outermost conn but finds N.EarlyWriter through
// wrappers such as group conns.
func (c *frameConn) NeedHandshakeForWrite() bool {
	return c.NeedHandshake()
}

func (c *frameConn) Upstream() any { return c.Conn }

func (c *frameConn) ReaderReplaceable() bool { return true }

// WriterReplaceable reports whether copy loops may bypass c, which they must not
// do until the frame is sent.
func (c *frameConn) WriterReplaceable() bool { return c.sent.Load() }
