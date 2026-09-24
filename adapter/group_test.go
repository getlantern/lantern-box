package adapter

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addressedPacketConn stands in for a per-packet-addressed outbound conn: it
// records each destination and, like shadowsocks, needs headroom around the
// payload.
type addressedPacketConn struct {
	net.PacketConn
	written  []M.Socksaddr
	payloads [][]byte
	readFrom net.Addr
}

const frontHeadroom, rearHeadroom = 32, 16

func (c *addressedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	if buffer.Start() < frontHeadroom || buffer.FreeLen() < rearHeadroom {
		return errors.New("insufficient headroom")
	}
	c.written = append(c.written, destination)
	c.payloads = append(c.payloads, append([]byte(nil), buffer.Bytes()...))
	return nil
}

func (c *addressedPacketConn) ReadPacket(*buf.Buffer) (M.Socksaddr, error) {
	return M.Socksaddr{}, errors.New("unused")
}

func (c *addressedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return copy(p, "ntp"), c.readFrom, nil
}

func (c *addressedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return 0, errors.New("WriteTo must not be used when WritePacket is available")
}

func (c *addressedPacketConn) FrontHeadroom() int { return frontHeadroom }
func (c *addressedPacketConn) RearHeadroom() int  { return rearHeadroom }

func TestTaggedPacketConn_WritePacketKeepsDomainDestination(t *testing.T) {
	inner := &addressedPacketConn{}
	tagged := NewTaggedPacketConn(inner, "a")
	require.Same(t, tagged, bufio.NewPacketConn(tagged),
		"sing must use the tagged conn directly rather than its net.PacketConn adapter")

	// A buffer with no headroom: the tagged conn must supply what inner needs.
	b := buf.New()
	_, err := b.WriteString("ntp")
	require.NoError(t, err)
	dst := M.ParseSocksaddr("time.android.com:123")
	require.NoError(t, tagged.WritePacket(b, dst))
	assert.Equal(t, []M.Socksaddr{dst}, inner.written, "inner must receive the domain destination")
}

func TestTaggedPacketConn_WritePacketLargePayloadWithoutHeadroom(t *testing.T) {
	// Supplying headroom must not truncate a payload larger than sing's
	// default UDP buffer, or the wrapped conn runs out of rear headroom.
	inner := &addressedPacketConn{}
	tagged := NewTaggedPacketConn(inner, "a")

	payload := bytes.Repeat([]byte{0xab}, buf.UDPBufferSize+1024)
	b := buf.NewSize(len(payload))
	_, err := b.Write(payload)
	require.NoError(t, err)
	require.NoError(t, tagged.WritePacket(b, M.ParseSocksaddr("1.1.1.1:53")))
	require.Len(t, inner.payloads, 1)
	assert.Equal(t, payload, inner.payloads[0], "the whole payload must reach the wrapped conn")
}

func TestTaggedPacketConn_ExposesWrappedHeadroom(t *testing.T) {
	// Callers size buffers from this walk; counting a layer twice or not at
	// all would mis-size every packet.
	tagged := NewTaggedPacketConn(&addressedPacketConn{}, "a")
	assert.Equal(t, frontHeadroom, N.CalculateFrontHeadroom(tagged))
	assert.Equal(t, rearHeadroom, N.CalculateRearHeadroom(tagged))
}

func TestTaggedPacketConn_NotReplaceable(t *testing.T) {
	// Copy loops unwrap only Reader/WriterReplaceable wrappers; the tagged
	// conn must stay in the path so the wrappers beneath it see every packet.
	tagged := NewTaggedPacketConn(&addressedPacketConn{}, "a")
	assert.Same(t, tagged, N.UnwrapPacketWriter(tagged))
	assert.Same(t, tagged, N.UnwrapPacketReader(tagged))
}

func TestTaggedPacketConn_ReadPacketKeepsDomainSource(t *testing.T) {
	src := M.ParseSocksaddr("time.android.com:123")
	tagged := NewTaggedPacketConn(&addressedPacketConn{readFrom: src}, "a")

	b := buf.New()
	defer b.Release()
	got, err := tagged.ReadPacket(b)
	require.NoError(t, err)
	assert.Equal(t, src, got)
	assert.Equal(t, "ntp", string(b.Bytes()))
}
