package e2e

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lboption "github.com/getlantern/lantern-box/option"
	tw "github.com/getlantern/twiddle"
)

// TestTwiddleCarriesTCPAndUDPOverOneTunnel exercises what the droplet e2e never
// has: the multiplexed outbound, and UDP through it.
//
// The shell e2e drives curl through a client box, which only ever opens TCP --
// it has no UDP coverage at all. And it predates muxing, so the one thing this
// transport now does differently on the wire (N inner destinations over one
// outer twiddle connection, UDP included via UoT) has never been run end to end.
//
// Both boxes run in-process, so this needs no droplet, no Docker and no
// credentials: what is actually under test is two lantern-box instances talking
// to each other, which is exactly what the remote host was standing in for.
func TestTwiddleCarriesTCPAndUDPOverOneTunnel(t *testing.T) {
	ctx := context.Background()
	boxCtx := box.BaseContext()

	key, err := tw.NewTicketKey()
	require.NoError(t, err)
	cover, err := tw.CoverFor("www.cloudflare.com")
	require.NoError(t, err)
	cred, err := key.Issue(1, cover.TicketLen)
	require.NoError(t, err)

	// The embedded pool would do, but naming it keeps the test honest about
	// which hellos are on the wire.
	pool := tw.FormatPool(tw.DefaultPool()[:1])

	serverPort := freePort(t)
	clientPort := freePort(t)

	serverOpts := twiddleOptions(boxCtx, t, "testdata/twiddle_server.json", map[string]string{
		"TICKET_KEY": hex.EncodeToString(key[:]),
	})
	clientOpts := twiddleOptions(boxCtx, t, "testdata/twiddle_client.json", map[string]string{
		"TICKET":     base64.StdEncoding.EncodeToString(cred.Ticket),
		"PSK":        hex.EncodeToString(cred.PSK[:]),
		"HELLO_POOL": strings.TrimSpace(pool),
	})
	serverOpts.Inbounds[0].Options.(*lboption.TwiddleInboundOptions).ListenPort = serverPort
	clientOpts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = clientPort
	clientOpts.Outbounds[0].Options.(*lboption.TwiddleOutboundOptions).ServerPort = serverPort

	serverBox, err := sbox.New(sbox.Options{Context: boxCtx, Options: serverOpts})
	require.NoError(t, err)
	require.NoError(t, serverBox.Start())
	t.Cleanup(func() { serverBox.Close() })

	clientBox, err := sbox.New(sbox.Options{Context: boxCtx, Options: clientOpts})
	require.NoError(t, err)
	require.NoError(t, clientBox.Start())
	t.Cleanup(func() { clientBox.Close() })

	client := socks.NewClient(
		N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")

	t.Run("tcp", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "through the tunnel")
		}))
		defer origin.Close()

		hc := &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return client.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			},
		}
		// Twice: the second request proves the tunnel is reusable, which under
		// muxing means a second stream rather than a second twiddle opening.
		for i := range 2 {
			resp, err := hc.Get(origin.URL)
			require.NoErrorf(t, err, "request %d", i+1)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, "through the tunnel", string(body))
		}
	})

	t.Run("udp", func(t *testing.T) {
		// A UDP echo origin: what comes back proves the datagram made the whole
		// round trip through UoT and the mux, not merely that a socket opened.
		origin, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer origin.Close()
		go func() {
			buf := make([]byte, 2048)
			for {
				n, addr, err := origin.ReadFrom(buf)
				if err != nil {
					return
				}
				_, _ = origin.WriteTo(buf[:n], addr)
			}
		}()

		conn, err := client.ListenPacket(ctx, M.ParseSocksaddr(origin.LocalAddr().String()))
		require.NoError(t, err, "SOCKS5 UDP associate through twiddle")
		defer conn.Close()
		require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))

		target := M.ParseSocksaddr(origin.LocalAddr().String()).UDPAddr()
		for i, payload := range [][]byte{
			[]byte("first datagram"),
			[]byte(strings.Repeat("x", 1200)),
		} {
			_, err = conn.WriteTo(payload, target)
			require.NoErrorf(t, err, "datagram %d", i+1)

			buf := make([]byte, 2048)
			n, _, err := conn.ReadFrom(buf)
			require.NoErrorf(t, err, "datagram %d reply", i+1)
			require.Equal(t, payload, buf[:n])
		}
	})
}

// twiddleOptions reads a config and substitutes the credentials generated for
// this run, so nothing secret-shaped is checked in and every run is independent.
func twiddleOptions(ctx context.Context, t *testing.T, path string, subs map[string]string) option.Options {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(raw)
	for k, v := range subs {
		require.Containsf(t, body, k, "%s has no %s placeholder", path, k)
		body = strings.ReplaceAll(body, k, v)
	}
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(body))
	require.NoError(t, err)
	return opts
}
