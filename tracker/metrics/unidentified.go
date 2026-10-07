package metrics

import (
	"context"
	"io"
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

func (u unidentifiedIO) add(n int64, opt metric.AddOption) {
	if n > 0 {
		metrics.unidentifiedIO.Add(context.Background(), n, opt)
	}
}

func (u unidentifiedIO) countRx(n int64) { u.add(n, u.rx) }
func (u unidentifiedIO) countTx(n int64) { u.add(n, u.tx) }

// unidentifiedConn counts the bytes of a connection without client info.
//
// It is a sing read/write counter: sing's copy loop unwraps it to the
// connection beneath (keeping that connection's ReadWaiter, vectorised and
// splice paths) and reports the bytes it moves through the count functions.
// Read and Write count only when something reads or writes the wrapper itself,
// so a byte is counted on one path or the other, never both.
type unidentifiedConn struct {
	net.Conn
	io unidentifiedIO
}

// newUnidentifiedConn wraps conn, keeping half-close exactly as conn has it:
// sing-box's connection manager half-closes a destination only if it is itself
// an N.WriteCloser, so the wrapper must be one precisely when conn is.
func newUnidentifiedConn(conn net.Conn, metadata adapter.InboundContext) net.Conn {
	c := &unidentifiedConn{Conn: conn, io: newUnidentifiedIO(metadata)}
	if wc, ok := conn.(N.WriteCloser); ok {
		return &unidentifiedDuplexConn{unidentifiedConn: c, closeWriter: wc}
	}
	return c
}

func (c *unidentifiedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.io.countRx(int64(n))
	return n, err
}

func (c *unidentifiedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.io.countTx(int64(n))
	return n, err
}

func (c *unidentifiedConn) UnwrapReader() (io.Reader, []N.CountFunc) {
	return c.Conn, []N.CountFunc{c.io.countRx}
}

func (c *unidentifiedConn) UnwrapWriter() (io.Writer, []N.CountFunc) {
	return c.Conn, []N.CountFunc{c.io.countTx}
}

func (c *unidentifiedConn) Upstream() any {
	return c.Conn
}

// unidentifiedDuplexConn is an unidentifiedConn over a connection that can
// half-close.
type unidentifiedDuplexConn struct {
	*unidentifiedConn
	closeWriter N.WriteCloser
}

func (c *unidentifiedDuplexConn) CloseWrite() error {
	return c.closeWriter.CloseWrite()
}

// unidentifiedPacketConn counts the bytes of a packet connection without
// client info. Like unidentifiedConn, it is a sing packet read/write counter.
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
		c.io.countRx(int64(buffer.Len()))
	}
	return dest, err
}

// WritePacket counts a packet only once it is written. Its length is read
// first because the writer may release the buffer.
func (c *unidentifiedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	n := buffer.Len()
	if err := c.PacketConn.WritePacket(buffer, destination); err != nil {
		return err
	}
	c.io.countTx(int64(n))
	return nil
}

func (c *unidentifiedPacketConn) UnwrapPacketReader() (N.PacketReader, []N.CountFunc) {
	return c.PacketConn, []N.CountFunc{c.io.countRx}
}

func (c *unidentifiedPacketConn) UnwrapPacketWriter() (N.PacketWriter, []N.CountFunc) {
	return c.PacketConn, []N.CountFunc{c.io.countTx}
}

func (c *unidentifiedPacketConn) Upstream() any {
	return c.PacketConn
}
