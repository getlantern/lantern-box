package clientcontext

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startUDPSink starts a UDP server that drains packets and sends no reply.
func startUDPSink(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	return conn.LocalAddr().(*net.UDPAddr)
}

func TestSendInfoWithIPDestination(t *testing.T) {
	serverAddr := startUDPSink(t)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	dest := M.SocksaddrFrom(netip.MustParseAddr(serverAddr.IP.String()), uint16(serverAddr.Port))

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err = wpc.sendInfo(conn)
	assert.NoError(t, err)
}

func TestSendInfoWithDomainAndResolvedAddresses(t *testing.T) {
	serverAddr := startUDPSink(t)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	// Simulate fakeip: destination is a domain, but DestinationAddresses has the resolved IP.
	dest := M.Socksaddr{Fqdn: "example.com", Port: uint16(serverAddr.Port)}

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{
			Destination:          dest,
			DestinationAddresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		},
		info: &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err = wpc.sendInfo(conn)
	assert.NoError(t, err)
}

type recordingPacketConn struct {
	writtenAddr net.Addr
}

func (c *recordingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n := copy(p, []byte("OK"))
	return n, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, nil
}

func (c *recordingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writtenAddr = addr
	return len(p), nil
}

func (c *recordingPacketConn) Close() error                       { return nil }
func (c *recordingPacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *recordingPacketConn) SetDeadline(_ time.Time) error      { return nil }
func (c *recordingPacketConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *recordingPacketConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestSendInfoWithDomainPassesThrough(t *testing.T) {
	conn := &recordingPacketConn{}

	dest := M.Socksaddr{Fqdn: "example.com", Port: 443}

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err := wpc.sendInfo(conn)
	require.NoError(t, err)
	assert.Equal(t, dest, conn.writtenAddr)
}

// stubConn records writes and fails reads so tests catch unexpected ack reads.
type stubConn struct {
	net.Conn
	written bytes.Buffer
	reads   int
}

func (c *stubConn) Write(p []byte) (int, error) { return c.written.Write(p) }

func (c *stubConn) Read([]byte) (int, error) {
	c.reads++
	return 0, errors.New("unexpected read")
}

func (c *stubConn) SetDeadline(time.Time) error { return nil }

func TestConnHandshakeSuccessDoesNotWaitForAck(t *testing.T) {
	server := &stubConn{}
	conn := newWriteConn(&stubConn{}, &ClientInfo{DeviceID: "test-device"}, boundsRule{}, nil).(*writeConn)

	require.NoError(t, conn.ConnHandshakeSuccess(server))

	assert.Zero(t, server.reads, "the handshake must not block on an acknowledgement")
	assert.Contains(t, server.written.String(), packetPrefix)
	assert.Contains(t, server.written.String(), "test-device")
}
