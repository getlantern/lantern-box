package metrics

import (
	"context"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PacketConn wraps a sing-box network.PacketConn and tracks metrics such as bytes sent and received.
type PacketConn struct {
	N.PacketConn
	attributes []attribute.KeyValue
	startTime  time.Time
}

// NewPacketConn creates a new PacketConn instance.
func NewPacketConn(conn N.PacketConn, metadata *adapter.InboundContext) N.PacketConn {
	attributes := metadataToAttributes(metadata)
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attributes...))

	return &PacketConn{
		PacketConn: conn,
		attributes: attributes,
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
		attrs := append(c.attributes, attribute.KeyValue{Key: "direction", Value: attribute.StringValue("receive")})
		metrics.proxyIO.Add(context.Background(), int64(buffer.Len()), metric.WithAttributes(attrs...))
	}
	return dest, nil
}

// WritePacket overrides network.PacketConn's WritePacket method to track sent bytes.
func (c *PacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if buffer.Len() > 0 {
		attrs := append(c.attributes, attribute.KeyValue{Key: "direction", Value: attribute.StringValue("transmit")})
		metrics.proxyIO.Add(context.Background(), int64(buffer.Len()), metric.WithAttributes(attrs...))
	}
	return c.PacketConn.WritePacket(buffer, destination)
}

// Close overrides net.PacketConn's Close method to track connection duration.
func (c *PacketConn) Close() error {
	duration := time.Since(c.startTime).Milliseconds()
	metrics.duration.Record(context.Background(), duration, metric.WithAttributes(c.attributes...))
	metrics.conns.Add(context.Background(), -1, metric.WithAttributes(c.attributes...))
	return c.PacketConn.Close()
}
