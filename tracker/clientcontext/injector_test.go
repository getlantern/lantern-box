package clientcontext

import (
	"net"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startUDPEchoOK starts a UDP server that expects a CLIENTINFO packet and responds "OK".
func startUDPEchoOK(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_ = n
			conn.WriteTo([]byte("OK"), addr)
		}
	}()

	return conn.LocalAddr().(*net.UDPAddr)
}

func TestSendInfoWithIPDestination(t *testing.T) {
	serverAddr := startUDPEchoOK(t)

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

func TestSendInfoWithDomainDestination(t *testing.T) {
	serverAddr := startUDPEchoOK(t)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	// Use a domain destination (simulating fakeip) — "localhost" resolves to 127.0.0.1
	dest := M.Socksaddr{Fqdn: "localhost", Port: uint16(serverAddr.Port)}

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err = wpc.sendInfo(conn)
	assert.NoError(t, err)
}

func TestSendInfoWithUnresolvableDomain(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	dest := M.Socksaddr{Fqdn: "this.domain.does.not.exist.invalid", Port: 12345}

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err = wpc.sendInfo(conn)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "resolving destination")
}
