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
