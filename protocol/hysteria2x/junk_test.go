//go:build with_quic

package hysteria2x_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/protocol/hysteria2x"
)

func TestNewJunkShape(t *testing.T) {
	for range 1000 {
		junk, err := hysteria2x.NewJunk()
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(junk), 8)
		require.LessOrEqual(t, len(junk), 64)
		require.NotZero(t, junk[0]&0x80, "long-header bit")
	}
}

// TestStockServerNeverAnswersJunk first confirms a stock hysteria2 inbound
// answers a full-size unknown-version packet, then sends it a few hundred junk
// datagrams and requires silence. Junk that answers would
// hand the censor a recognizable server datagram (for example a Version
// Negotiation) on a flow meant to open with unparseable bytes. Random DCID
// lengths make many of these datagrams parse as long headers, so this exercises
// the version-negotiation path, not just malformed input.
func TestStockServerNeverAnswersJunk(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	ctx := box.BaseContext()
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(`{
		"log": {"level": "error"},
		"inbounds": [{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": %d,
			"users": [{"password": "pw"}], "tls": {"enabled": true, "insecure": true}}]
	}`, port)))
	require.NoError(t, err)
	server, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() { server.Close() })

	// Control: a full-size unknown-version packet must draw a Version
	// Negotiation, or silence below proves nothing about the server listening.
	control, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err)
	pkt := make([]byte, 1300)
	pkt[0] = 0xc0
	copy(pkt[1:5], []byte{0x1a, 0x2a, 0x3a, 0x4a})
	pkt[5] = 8  // DCID length
	pkt[14] = 8 // SCID length
	_, err = control.Write(pkt)
	require.NoError(t, err)
	require.NoError(t, control.SetReadDeadline(time.Now().Add(2*time.Second)))
	vn := make([]byte, 2048)
	n, err := control.Read(vn)
	control.Close()
	require.NoError(t, err, "control: server must answer a 1300-byte unknown-version packet")
	require.GreaterOrEqual(t, n, 5)
	require.Equal(t, []byte{0, 0, 0, 0}, vn[1:5], "control reply must be a Version Negotiation")

	for flow := range 20 {
		conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		require.NoError(t, err)
		for range 25 {
			junk, err := hysteria2x.NewJunk()
			require.NoError(t, err)
			_, err = conn.Write(junk)
			require.NoError(t, err)
		}
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
		buf := make([]byte, 2048)
		n, err := conn.Read(buf)
		conn.Close()
		require.Errorf(t, err, "flow %d: server answered junk with %d bytes: %x", flow, n, buf[:n])
		var ne net.Error
		require.ErrorAs(t, err, &ne)
		require.True(t, ne.Timeout(), "flow %d: expected silence, got %v", flow, err)
	}
}
