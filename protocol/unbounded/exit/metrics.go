package exit

import (
	"context"
	"net"
	"sync"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// exitMetrics counts the exit's sessions, refusals and bytes. It never takes an address or a
// host as an attribute.
type exitMetrics struct {
	// provider is the meter provider; nil means the global one, looked up on first use so
	// that it is whatever the embedder installed.
	provider metric.MeterProvider

	once     sync.Once
	sessions metric.Int64Counter
	refusals metric.Int64Counter
	io       metric.Int64Counter
}

var (
	pathICE    = metric.WithAttributeSet(attribute.NewSet(attribute.String("path", "ice")))
	pathWebRTC = metric.WithAttributeSet(attribute.NewSet(attribute.String("path", "webrtc")))
	ioUp       = metric.WithAttributeSet(attribute.NewSet(attribute.String("direction", "up")))
	ioDown     = metric.WithAttributeSet(attribute.NewSet(attribute.String("direction", "down")))
)

func newExitMetrics() *exitMetrics { return &exitMetrics{} }

func (m *exitMetrics) init() {
	m.once.Do(func() {
		p := m.provider
		if p == nil {
			p = otel.GetMeterProvider()
		}
		meter := p.Meter("lantern-box/unbounded-exit")
		fallback := noop.Meter{}
		var err error
		if m.sessions, err = meter.Int64Counter("unbounded.exit.sessions",
			metric.WithDescription("Volunteer sessions connected, by whether they exit directly (ice) or relay to the egress server (webrtc)")); err != nil {
			m.sessions, _ = fallback.Int64Counter("")
		}
		if m.refusals, err = meter.Int64Counter("unbounded.exit.refused",
			metric.WithDescription("Direct exit connections the exit box's rules refused")); err != nil {
			m.refusals, _ = fallback.Int64Counter("")
		}
		if m.io, err = meter.Int64Counter("unbounded.exit.io",
			metric.WithDescription("Bytes a volunteer's direct exit carried, up from users and down to them"),
			metric.WithUnit("By")); err != nil {
			m.io, _ = fallback.Int64Counter("")
		}
	})
}

// session is clientcore's OnSession hook.
func (m *exitMetrics) session(directExit bool) {
	m.init()
	if directExit {
		m.sessions.Add(context.Background(), 1, pathICE)
	} else {
		m.sessions.Add(context.Background(), 1, pathWebRTC)
	}
}

func (m *exitMetrics) refused() {
	m.init()
	m.refusals.Add(context.Background(), 1)
}

func (m *exitMetrics) bytes(n int, direction metric.AddOption) {
	if n <= 0 {
		return
	}
	m.init()
	m.io.Add(context.Background(), int64(n), direction)
}

// countConn counts a routed stream's bytes: what the router reads from it came up from the
// user, what it writes goes down to them.
func (m *exitMetrics) countConn(conn net.Conn) net.Conn {
	return &countedConn{Conn: conn, m: m}
}

type countedConn struct {
	net.Conn
	m *exitMetrics
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.m.bytes(n, ioUp)
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.m.bytes(n, ioDown)
	return n, err
}

// CloseWrite keeps half-close working through the wrapper.
func (c *countedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Upstream lets sing find the stream's handshake reporting. Without ReaderReplaceable, its
// copy never unwraps past the counter.
func (c *countedConn) Upstream() any { return c.Conn }

// countPacketConn counts a routed flow's bytes as countConn does.
func (m *exitMetrics) countPacketConn(conn N.PacketConn) N.PacketConn {
	return &countedPacketConn{PacketConn: conn, m: m}
}

type countedPacketConn struct {
	N.PacketConn
	m *exitMetrics
}

func (c *countedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	destination, err := c.PacketConn.ReadPacket(buffer)
	if err == nil {
		c.m.bytes(buffer.Len(), ioUp)
	}
	return destination, err
}

func (c *countedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	// WritePacket releases the buffer, so count it first.
	n := buffer.Len()
	err := c.PacketConn.WritePacket(buffer, destination)
	if err == nil {
		c.m.bytes(n, ioDown)
	}
	return err
}

func (c *countedPacketConn) Upstream() any { return c.PacketConn }
