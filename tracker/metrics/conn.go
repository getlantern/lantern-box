package metrics

import (
	"net"
	"sync/atomic"
	"time"
)

// Conn wraps a net.Conn and tracks metrics such as bytes sent and received.
type Conn struct {
	net.Conn
	attrs     *attributes
	tracker   *MetricsTracker
	startTime time.Time
	rxBytes   atomic.Int64
	closed    atomic.Bool
}

// NewConn creates a new Conn instance.
func NewConn(conn net.Conn, attrs *attributes, tracker *MetricsTracker) net.Conn {
	return &Conn{
		Conn:      conn,
		attrs:     attrs,
		tracker:   tracker,
		startTime: time.Now(),
	}
}

// Read overrides net.Conn's Read method to track received bytes.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.rxBytes.Add(int64(n))
		c.tracker.TrackIO(rx, n, c.attrs)
	}
	return
}

// Write overrides net.Conn's Write method to track sent bytes.
func (c *Conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		c.tracker.TrackIO(tx, n, c.attrs)
	}
	return
}

// Close records the session's metrics exactly once, then forwards the close.
// sing-box closes a routed conn up to 3x on non-graceful teardown; the guard
// stops the connections gauge and duration/goodput from being counted repeatedly.
func (c *Conn) Close() error {
	if !c.closed.Swap(true) {
		duration := time.Since(c.startTime).Milliseconds()
		c.tracker.Leave(duration, c.attrs)
		c.tracker.recordGoodput(c.rxBytes.Load(), duration, c.attrs)
	}
	return c.Conn.Close()
}

// CloseWrite implements N.WriteCloser to support half-close semantics.
// This is critical for protocols tunneled over H2 (like Samizdat) where
// sing-box's bidirectional copy needs to half-close one direction without
// killing the entire stream.
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *Conn) Upstream() any {
	return c.Conn
}
