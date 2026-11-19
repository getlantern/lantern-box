// Package blackhole is a UDP conn that drops everything, for the QUIC an outbound's exit
// would refuse.
package blackhole

import (
	"net"
	"os"
	"sync"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// Conn is the conn for UDP to port 443, which is QUIC, where an outbound's
// exit may admit only allowlisted hostnames: QUIC carries none we can rely
// on. It never touches the outbound's transport, discards writes, and blocks
// reads until Close or a read deadline, so the browser falls back to TCP,
// which carries the sniffed hostname. Dropping silently rather than returning
// an error keeps a group from counting a policy decision as the outbound's
// failure.
type Conn struct {
	remote    M.Socksaddr
	closed    chan struct{}
	closeOnce sync.Once

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	// readWake is closed and replaced whenever the read deadline changes,
	// so a blocked read rechecks it.
	readWake chan struct{}
}

var (
	_ net.Conn       = (*Conn)(nil)
	_ net.PacketConn = (*Conn)(nil)
)

// New returns a black hole whose RemoteAddr is remote.
func New(remote M.Socksaddr) *Conn {
	return &Conn{remote: remote, closed: make(chan struct{}), readWake: make(chan struct{})}
}

func (c *Conn) Read([]byte) (int, error) { return 0, c.waitRead() }

func (c *Conn) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, c.waitRead() }

func (c *Conn) waitRead() error {
	for {
		c.mu.Lock()
		deadline, wake := c.readDeadline, c.readWake
		c.mu.Unlock()
		select {
		case <-c.closed:
			return net.ErrClosed
		default:
		}
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			expired = timer.C
		}
		var err error
		select {
		case <-c.closed:
			err = net.ErrClosed
		case <-expired:
			err = os.ErrDeadlineExceeded
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if err != nil {
			return err
		}
	}
}

func (c *Conn) Write(b []byte) (int, error) { return c.write(b) }

func (c *Conn) WriteTo(b []byte, _ net.Addr) (int, error) { return c.write(b) }

func (c *Conn) write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return len(b), nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *Conn) LocalAddr() net.Addr  { return M.Socksaddr{} }
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

func (c *Conn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	close(c.readWake)
	c.readWake = make(chan struct{})
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}
