package metrics

import (
	"context"
	"net"

	semconv "github.com/getlantern/semconv"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Connections that carry no client info are relayed like any other but are
// invisible to proxy.io and to the datacap tracker, both of which only see
// identified clients. These metrics count them, so the share of a proxy's
// traffic that nothing attributes is measurable. They are separate from
// proxy.io so that proxy.io keeps meaning "identified client traffic".
//
// Their point attributes are deliberately few, since an unidentified
// connection has no client attributes to add: the inbound, its type, the
// protocol, the direction for bytes, and the bare "track" key, which is
// queryable where the "proxy.track" resource attribute is not (see
// recordGoodput).

// unidentifiedAttrs is the attribute set shared by every unidentified metric
// for one connection.
func unidentifiedAttrs(metadata adapter.InboundContext) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.NetworkProtocolNameKey.String(metadata.Protocol),
		semconv.ProxyInboundKey.String(metadata.Inbound),
		semconv.ProxyInboundTypeKey.String(metadata.InboundType),
		attribute.String("track", metrics.track),
	}
}

// unidentifiedIO holds one connection's precomputed byte-counter options, one
// per direction, so counting a read or write builds no attributes.
type unidentifiedIO struct {
	rx, tx metric.AddOption
}

// newUnidentifiedIO counts a new unidentified connection and returns the byte
// counters for it.
func newUnidentifiedIO(metadata adapter.InboundContext) unidentifiedIO {
	attrs := unidentifiedAttrs(metadata)
	metrics.unidentifiedConns.Add(context.Background(), 1, metric.WithAttributeSet(attribute.NewSet(attrs...)))
	withDirection := func(d ioAttr) metric.AddOption {
		return metric.WithAttributeSet(attribute.NewSet(append(attrs[:len(attrs):len(attrs)], semconv.NetworkIODirectionKey.String(string(d)))...))
	}
	return unidentifiedIO{rx: withDirection(rx), tx: withDirection(tx)}
}

func (u unidentifiedIO) add(n int, opt metric.AddOption) {
	if n > 0 {
		metrics.unidentifiedIO.Add(context.Background(), int64(n), opt)
	}
}

// unidentifiedConn counts the bytes of a connection without client info.
type unidentifiedConn struct {
	net.Conn
	io unidentifiedIO
}

func newUnidentifiedConn(conn net.Conn, metadata adapter.InboundContext) net.Conn {
	return &unidentifiedConn{Conn: conn, io: newUnidentifiedIO(metadata)}
}

func (c *unidentifiedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.io.add(n, c.io.rx)
	return n, err
}

func (c *unidentifiedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.io.add(n, c.io.tx)
	return n, err
}

// CloseWrite forwards half-close, as Conn does.
func (c *unidentifiedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *unidentifiedConn) Upstream() any {
	return c.Conn
}

// unidentifiedPacketConn counts the bytes of a packet connection without
// client info.
type unidentifiedPacketConn struct {
	N.PacketConn
	io unidentifiedIO
}

func newUnidentifiedPacketConn(conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	return &unidentifiedPacketConn{PacketConn: conn, io: newUnidentifiedIO(metadata)}
}

func (c *unidentifiedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err == nil {
		c.io.add(buffer.Len(), c.io.rx)
	}
	return dest, err
}

func (c *unidentifiedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	c.io.add(buffer.Len(), c.io.tx)
	return c.PacketConn.WritePacket(buffer, destination)
}

func (c *unidentifiedPacketConn) Upstream() any {
	return c.PacketConn
}
