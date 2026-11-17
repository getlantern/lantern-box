//go:build with_quic

package e2e

import (
	"cmp"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

// TestHysteria2XLiveQUICSites sends real HTTP/3 requests to QUIC sites
// (YouTube, Google) through a hysteria2x client and a stock hysteria2 server,
// so the inner QUIC rides the hysteria2 UDP relay. It reaches the internet, so
// it only runs with QUIC_LIVE=1.
func TestHysteria2XLiveQUICSites(t *testing.T) {
	if os.Getenv("QUIC_LIVE") == "" {
		t.Skip("set QUIC_LIVE=1 to run against live QUIC sites")
	}
	sites := []string{
		"https://www.youtube.com/",
		"https://www.google.com/generate_204",
		"https://i.ytimg.com/generate_204",
		"https://cloudflare-quic.com/",
	}

	t.Run("direct-control", func(t *testing.T) {
		for _, u := range sites {
			proto, n, err := h3Get(t, u, nil)
			t.Logf("direct %s → proto=%s bytes=%d err=%v", u, proto, n, err)
		}
	})

	for _, junk := range []bool{false, true} {
		t.Run(fmt.Sprintf("pre_initial_junk=%t", junk), func(t *testing.T) {
			boxCtx := box.BaseContext()
			serverPort := freeUDPPort(t)
			clientPort := freePort(t)
			startBox(t, boxCtx, mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": %d,
					"users": [{"password": "pw"}], "tls": {"enabled": true, "insecure": true}}],
				"outbounds": [{"type": "direct"}]
			}`, serverPort)))
			startBox(t, boxCtx, mustOptions(boxCtx, t, fmt.Sprintf(`{
				"log": {"level": "error"},
				"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
				"outbounds": [{"type": "hysteria2x", "server": "127.0.0.1", "server_port": %d,
					"password": "pw", "pre_initial_junk": %t,
					"tls": {"enabled": true, "server_name": "example.com", "insecure": true}}]
			}`, clientPort, serverPort, junk)))

			sc := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
			for _, u := range sites {
				proto, n, err := h3Get(t, u, sc)
				require.NoError(t, err, "%s over hysteria2x", u)
				require.Equal(t, "HTTP/3.0", proto)
				t.Logf("hysteria2x %s → proto=%s bytes=%d", u, proto, n)
			}
		})
	}
}

// h3Get fetches u over HTTP/3 only. With sc set, the QUIC packets go through
// the SOCKS5 UDP ASSOCIATE of the hysteria2x client's mixed inbound.
func h3Get(t *testing.T, u string, sc *socks.Client) (string, int, error) {
	t.Helper()
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{},
		Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			if sc == nil {
				return quic.DialAddrEarly(ctx, addr, tlsCfg, cfg)
			}
			ua, err := net.ResolveUDPAddr("udp4", addr)
			if err != nil {
				return nil, err
			}
			pc, err := sc.ListenPacket(ctx, M.SocksaddrFromNet(ua))
			if err != nil {
				return nil, err
			}
			return quic.DialEarly(ctx, pc, ua, tlsCfg, cfg)
		},
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.Proto, len(b), err
}

// TestOutlineLiveQUICSites sends real HTTP/3 requests to QUIC sites straight
// from the proxyless outline outbound, with no proxy server. OUTLINE_UDP sets
// the outbound's udp option and defaults to a single quicprelude decoy. It
// reaches the internet, so it only runs with QUIC_LIVE=1.
func TestOutlineLiveQUICSites(t *testing.T) {
	if os.Getenv("QUIC_LIVE") == "" {
		t.Skip("set QUIC_LIVE=1 to run against live QUIC sites")
	}
	boxCtx := box.BaseContext()
	clientPort := freePort(t)
	startBox(t, boxCtx, mustOptions(boxCtx, t, fmt.Sprintf(`{
		"log": {"level": "error"},
		"inbounds": [{"type": "mixed", "listen": "127.0.0.1", "listen_port": %d}],
		"outbounds": [{"type": "outline", "domains": ["example.com"], "udp": %q}]
	}`, clientPort, cmp.Or(os.Getenv("OUTLINE_UDP"), "quicprelude:count=1"))))
	sc := socks.NewClient(N.SystemDialer, M.ParseSocksaddrHostPort("127.0.0.1", clientPort), socks.Version5, "", "")
	for _, u := range []string{"https://www.youtube.com/", "https://i.ytimg.com/generate_204", "https://www.google.com/generate_204", "https://cloudflare-quic.com/"} {
		proto, n, err := h3Get(t, u, sc)
		require.NoError(t, err, u)
		require.Equal(t, "HTTP/3.0", proto)
		t.Logf("outline %s → proto=%s bytes=%d", u, proto, n)
	}
}
