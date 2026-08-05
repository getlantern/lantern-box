package clientcontext

import (
	"net"
	"net/netip"
	"testing"
	"time"

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

func TestSendInfoWithDomainAndResolvedAddresses(t *testing.T) {
	serverAddr := startUDPEchoOK(t)

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

func TestSendInfoWithDomainFallsBackToDNS(t *testing.T) {
	serverAddr := startUDPEchoOK(t)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	// Domain destination with no DestinationAddresses — falls back to DNS resolution.
	// "localhost" resolves to 127.0.0.1 so this reaches our echo server.
	dest := M.Socksaddr{Fqdn: "localhost", Port: uint16(serverAddr.Port)}

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}

	err = wpc.sendInfo(conn)
	assert.NoError(t, err)
}

func TestSendInfoWithUnresolvableDomainFails(t *testing.T) {
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

func setSendInfoTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := sendInfoTimeout
	sendInfoTimeout = d
	t.Cleanup(func() { sendInfoTimeout = old })
}

func TestStreamSendInfoReadsSplitOK(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		buf := make([]byte, 4096)
		server.Read(buf)
		// "OK" delivered across two reads; sendInfo must not treat a short
		// read as an invalid response.
		server.Write([]byte("O"))
		server.Write([]byte("K"))
	}()

	wc := &writeConn{info: &ClientInfo{DeviceID: "test-device", Platform: "test"}}
	assert.NoError(t, wc.sendInfo(client))
}

func TestStreamSendInfoTimesOutWithoutResponse(t *testing.T) {
	setSendInfoTimeout(t, 100*time.Millisecond)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		buf := make([]byte, 4096)
		server.Read(buf)
		// never respond
	}()

	wc := &writeConn{info: &ClientInfo{DeviceID: "test-device", Platform: "test"}}
	start := time.Now()
	err := wc.sendInfo(client)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestStreamSendInfoClearsDeadlineAfterSuccess(t *testing.T) {
	setSendInfoTimeout(t, 100*time.Millisecond)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		buf := make([]byte, 4096)
		server.Read(buf)
		server.Write([]byte("OK"))
	}()

	wc := &writeConn{info: &ClientInfo{DeviceID: "test-device", Platform: "test"}}
	require.NoError(t, wc.sendInfo(client))

	// The conn is piped for the connection's lifetime after sendInfo; a
	// leftover deadline would kill reads that outlast the timeout.
	go func() {
		time.Sleep(300 * time.Millisecond)
		server.Write([]byte("data"))
	}()
	buf := make([]byte, 4)
	_, err := client.Read(buf)
	assert.NoError(t, err)
}

func TestPacketSendInfoAcceptsOversizedResponse(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { server.Close() })

	go func() {
		buf := make([]byte, 4096)
		_, addr, err := server.ReadFrom(buf)
		if err != nil {
			return
		}
		server.WriteTo([]byte("OK with trailing transport overhead"), addr)
	}()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	serverAddr := server.LocalAddr().(*net.UDPAddr)
	dest := M.SocksaddrFrom(netip.MustParseAddr(serverAddr.IP.String()), uint16(serverAddr.Port))

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}
	assert.NoError(t, wpc.sendInfo(conn))
}

func TestPacketSendInfoTimesOutWithoutResponse(t *testing.T) {
	setSendInfoTimeout(t, 100*time.Millisecond)

	// Server reads but never responds.
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { server.Close() })

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()

	serverAddr := server.LocalAddr().(*net.UDPAddr)
	dest := M.SocksaddrFrom(netip.MustParseAddr(serverAddr.IP.String()), uint16(serverAddr.Port))

	wpc := &writePacketConn{
		metadata: adapter.InboundContext{Destination: dest},
		info:     &ClientInfo{DeviceID: "test-device", Platform: "test"},
	}
	start := time.Now()
	err = wpc.sendInfo(conn)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}
