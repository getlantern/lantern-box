//go:build with_quic

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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
)

// TestHysteria2XPreInitialJunk runs a hysteria2x client against a stock
// hysteria2 inbound through a UDP relay that records what the client puts on
// the wire. With pre_initial_junk the flow's first datagram must be the junk and
// the second the QUIC Initial, and the stock server must still accept the
// connection. Without it the first datagram must be the Initial.
func TestHysteria2XPreInitialJunk(t *testing.T) {
	for _, junk := range []bool{true, false} {
		t.Run(fmt.Sprintf("pre_initial_junk=%t", junk), func(t *testing.T) {
			boxCtx := box.BaseContext()
			serverPort := freeUDPPort(t)
			relay := startRecordingUDPRelay(t, serverPort)
			clientPort := freePort(t)

			serverOpts := mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": %d,
					"users": [{"password": "pw"}], "tls": {"enabled": true, "insecure": true}}],
				"outbounds": [{"type": "direct"}]
			}`, serverPort))
			clientOpts := mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
				"outbounds": [{"type": "hysteria2x", "server": "127.0.0.1", "server_port": %d,
					"password": "pw", "pre_initial_junk": %t,
					"tls": {"enabled": true, "server_name": "example.com", "insecure": true}}]
			}`, clientPort, relay.port, junk))

			startBox(t, boxCtx, serverOpts)
			startBox(t, boxCtx, clientOpts)

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "through hysteria2x")
			}))
			defer origin.Close()
			client := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
			hc := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return client.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			}}
			resp, err := hc.Get(origin.URL)
			require.NoError(t, err, "stock hysteria2 server must accept the connection")
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, "through hysteria2x", string(body))

			first := relay.firstDatagrams(2)
			require.Len(t, first, 2)
			if junk {
				require.GreaterOrEqual(t, len(first[0]), 8)
				require.LessOrEqual(t, len(first[0]), 64)
				require.NotZero(t, first[0][0]&0x80, "junk must carry the long-header bit")
				require.True(t, isQUICv1Initial(first[1]), "the Initial must follow the junk")
			} else {
				require.True(t, isQUICv1Initial(first[0]), "without junk the Initial comes first")
			}
		})
	}
}

// isQUICv1Initial reports whether b is a long-header QUIC v1 Initial padded to
// the RFC 9000 minimum.
func isQUICv1Initial(b []byte) bool {
	return len(b) >= 1200 && b[0]&0xf0 == 0xc0 &&
		b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 1
}

func mustOptions(ctx context.Context, t *testing.T, raw string) option.Options {
	t.Helper()
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(raw))
	require.NoError(t, err)
	return opts
}

func startBox(t *testing.T, ctx context.Context, opts option.Options) {
	t.Helper()
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, b.Start())
	t.Cleanup(func() { b.Close() })
}

func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer c.Close()
	return uint16(c.LocalAddr().(*net.UDPAddr).Port)
}

// recordingUDPRelay forwards one client flow to the server and keeps a copy of
// every client-to-server datagram, in order.
type recordingUDPRelay struct {
	port uint16
	mu   sync.Mutex
	sent [][]byte
}

func startRecordingUDPRelay(t *testing.T, serverPort uint16) *recordingUDPRelay {
	t.Helper()
	front, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	back, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(serverPort)})
	require.NoError(t, err)
	t.Cleanup(func() { front.Close(); back.Close() })

	r := &recordingUDPRelay{port: uint16(front.LocalAddr().(*net.UDPAddr).Port)}
	var clientAddr net.Addr
	var addrMu sync.Mutex
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := front.ReadFrom(buf)
			if err != nil {
				return
			}
			addrMu.Lock()
			clientAddr = addr
			addrMu.Unlock()
			r.mu.Lock()
			r.sent = append(r.sent, append([]byte(nil), buf[:n]...))
			r.mu.Unlock()
			_, _ = back.Write(buf[:n])
		}
	}()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := back.Read(buf)
			if err != nil {
				return
			}
			addrMu.Lock()
			addr := clientAddr
			addrMu.Unlock()
			if addr != nil {
				_, _ = front.WriteTo(buf[:n], addr)
			}
		}
	}()
	return r
}

func (r *recordingUDPRelay) firstDatagrams(n int) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent[:min(n, len(r.sent))]
}
