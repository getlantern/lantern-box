package metrics

import (
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// PacketConn wraps a sing-box network.PacketConn and tracks metrics such as bytes sent and received.
type PacketConn struct {
	N.PacketConn
	attrs     *attributes
	tracker   *MetricsTracker
	startTime time.Time
	rxBytes   atomic.Int64
	closed    atomic.Bool
}

// NewPacketConn creates a new PacketConn instance.
func NewPacketConn(conn N.PacketConn, attrs *attributes, tracker *MetricsTracker) N.PacketConn {
	return &PacketConn{
		PacketConn: conn,
		attrs:      attrs,
		tracker:    tracker,
		startTime:  time.Now(),
	}
}

// ReadPacket overrides network.PacketConn's ReadPacket method to track received bytes.
func (c *PacketConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}
	if buffer.Len() > 0 {
		c.rxBytes.Add(int64(buffer.Len()))
		c.tracker.TrackIO(rx, buffer.Len(), c.attrs)
	}
	return dest, nil
}

// WritePacket overrides network.PacketConn's WritePacket method to track sent bytes.
func (c *PacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if buffer.Len() > 0 {
		c.tracker.TrackIO(tx, buffer.Len(), c.attrs)
	}
	return c.PacketConn.WritePacket(buffer, destination)
}

// Close records the session's metrics exactly once, then forwards the close.
// See Conn.Close.
func (c *PacketConn) Close() error {
	if !c.closed.Swap(true) {
		duration := time.Since(c.startTime).Milliseconds()
		c.tracker.Leave(duration, c.attrs)
		c.tracker.recordGoodput(c.rxBytes.Load(), duration, c.attrs)
	}
	return c.PacketConn.Close()
}

func (c *PacketConn) Upstream() any {
	return c.PacketConn
}
