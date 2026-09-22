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

// TestStockServerNeverAnswersJunk sends a few hundred junk datagrams straight
// to a stock hysteria2 inbound and requires silence. Junk that answers would
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
