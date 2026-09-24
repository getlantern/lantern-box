package adapter

import (
	"errors"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// ErrGroupClosed is returned by MutableOutboundGroup Add/Remove when the
// group has already been torn down.
var ErrGroupClosed = errors.New("group is closed")

type MutableOutboundGroup interface {
	adapter.OutboundGroup
	Add(tags ...string) (n int, err error)
	Remove(tags ...string) (n int, err error)
}

// URLOverrideSetter is implemented by outbound groups that support per-outbound URL test overrides.
type URLOverrideSetter interface {
	SetURLOverrides(overrides map[string]string)
}

// OutboundChecker is implemented by outbound groups that support on-demand URL testing.
type OutboundChecker interface {
	CheckOutbounds()
}

// ExhaustionSignaler exposes a channel the host can select on to learn that
// the group's reconnection ladder finished without finding a working
// candidate. At most one value is sent per ladder run; the host decides
// what to do (typically a rate-limited config refetch).
type ExhaustionSignaler interface {
	ExhaustionSignal() <-chan struct{}
}

// TaggedConn is a net.Conn tagged with the outbound tag used to create it.
type TaggedConn struct {
	net.Conn
	outboundTag string
}

func NewTaggedConn(conn net.Conn, outboundTag string) *TaggedConn {
	return &TaggedConn{
		Conn:        conn,
		outboundTag: outboundTag,
	}
}

func (c *TaggedConn) Tag() string {
	return c.outboundTag
}

// TaggedPacketConn is a net.PacketConn tagged with the outbound tag used to create it.
//
// It implements N.NetPacketConn so packets written to a domain-name destination
// reach the wrapped conn with the domain intact. Written only as a
// net.PacketConn, sing's bufio adapter would reduce the destination to an
// IP-only address, which per-packet-addressed outbounds reject.
type TaggedPacketConn struct {
	net.PacketConn
	writer      N.PacketWriter
	outboundTag string
}

func NewTaggedPacketConn(conn net.PacketConn, outboundTag string) *TaggedPacketConn {
	return &TaggedPacketConn{
		PacketConn:  conn,
		writer:      bufio.NewPacketConn(conn),
		outboundTag: outboundTag,
	}
}

func (c *TaggedPacketConn) Tag() string {
	return c.outboundTag
}

func (c *TaggedPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(c.PacketConn)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

func (c *TaggedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	// The wrapped conn writes into buffer's headroom; a caller that sized
	// the buffer without it would make the write panic. The copy is sized
	// from the payload because sing's fixed-size buffers would truncate a
	// large one.
	front, rear := N.CalculateFrontHeadroom(c.PacketConn), N.CalculateRearHeadroom(c.PacketConn)
	if buffer.Start() < front || buffer.FreeLen() < rear {
		sized := buf.NewSize(front + buffer.Len() + rear)
		sized.Resize(front, 0)
		sized.Write(buffer.Bytes())
		buffer.Release()
		buffer = sized
	}
	return c.writer.WritePacket(buffer, destination)
}

// Upstream exposes the wrapped conn to common.Cast and headroom/MTU lookups.
// TaggedPacketConn is deliberately not Reader/WriterReplaceable, so copy
// loops never bypass it or the wrappers beneath it.
func (c *TaggedPacketConn) Upstream() any {
	return c.PacketConn
}

var _ N.NetPacketConn = (*TaggedPacketConn)(nil)
