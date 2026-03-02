package adapter

import (
	"net"

	"github.com/sagernet/sing-box/adapter"
)

type MutableOutboundGroup interface {
	adapter.OutboundGroup
	Add(tags ...string) (n int, err error)
	Remove(tags ...string) (n int, err error)
}

// URLOverrideSetter is implemented by outbound groups that support per-outbound URL test overrides.
type URLOverrideSetter interface {
	SetURLOverrides(overrides map[string]string)
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
type TaggedPacketConn struct {
	net.PacketConn
	outboundTag string
}

func NewTaggedPacketConn(conn net.PacketConn, outboundTag string) *TaggedPacketConn {
	return &TaggedPacketConn{
		PacketConn:  conn,
		outboundTag: outboundTag,
	}
}

func (c *TaggedPacketConn) Tag() string {
	return c.outboundTag
}
