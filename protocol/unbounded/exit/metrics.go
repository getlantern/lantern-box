package exit

import (
	"context"
	"net"
	"reflect"
	"sync/atomic"

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
	// provider is the meter provider; nil means the global one, looked up at each measurement
	// so that it is whatever the embedder installed last.
	provider metric.MeterProvider

	// current holds the instruments of the provider they were made from.
	current atomic.Pointer[exitInstruments]
}

type exitInstruments struct {
	provider metric.MeterProvider
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

// instruments returns the current provider's instruments, making them when the provider has
// changed since the last measurement, as protocol/group's currentHedgeTelemetry does: an
// embedder that swaps or shuts down its provider would otherwise lose every later measurement.
func (m *exitMetrics) instruments() *exitInstruments {
	p := m.provider
	if p == nil {
		p = otel.GetMeterProvider()
	}
	// A provider whose type can't be compared would make == panic; it gets fresh instruments
	// at every measurement instead.
	comparable := reflect.TypeOf(p).Comparable()
	if cur := m.current.Load(); cur != nil && comparable && cur.provider == p {
		return cur
	}
	meter := p.Meter("lantern-box/unbounded-exit")
	fallback := noop.Meter{}
	in := &exitInstruments{provider: p}
	var err error
	if in.sessions, err = meter.Int64Counter("unbounded.exit.sessions",
		metric.WithDescription("Volunteer sessions connected, by whether they exit directly (ice) or relay to the egress server (webrtc)")); err != nil {
		in.sessions, _ = fallback.Int64Counter("")
	}
	if in.refusals, err = meter.Int64Counter("unbounded.exit.refused",
		metric.WithDescription("Direct exit connections the exit box's rules refused")); err != nil {
		in.refusals, _ = fallback.Int64Counter("")
	}
	if in.io, err = meter.Int64Counter("unbounded.exit.io",
		metric.WithDescription("Bytes a volunteer's direct exit carried, up from users and down to them"),
		metric.WithUnit("By")); err != nil {
		in.io, _ = fallback.Int64Counter("")
	}
	m.current.Store(in)
	return in
}

// session is clientcore's OnSession hook.
func (m *exitMetrics) session(directExit bool) {
	in := m.instruments()
	if directExit {
		in.sessions.Add(context.Background(), 1, pathICE)
	} else {
		in.sessions.Add(context.Background(), 1, pathWebRTC)
	}
}

func (m *exitMetrics) refused() {
	m.instruments().refusals.Add(context.Background(), 1)
}

func (m *exitMetrics) bytes(n int, direction metric.AddOption) {
	if n <= 0 {
		return
	}
	m.instruments().io.Add(context.Background(), int64(n), direction)
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

// CloseWrite keeps half-close working through the wrapper. A stream that can't half-close is
// closed, as sing's copy closes a destination that isn't an N.WriteCloser: otherwise the user
// never sees the end of a response its server ended by closing.
func (c *countedConn) CloseWrite() error {
	if cw, ok := c.Conn.(N.WriteCloser); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
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
