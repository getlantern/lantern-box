//go:build integration

package water

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/refraction-networking/water/transport/v1"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

func TestOutboundWASM(t *testing.T) {
	server := startTestHTTPServer(t)
	t.Parallel()
	_, ctx := startBoxServer(t, `
		{
			"log": {
				"level": "trace",
				"output": "stdout",
				"timestamp": true
			},
			"inbounds": [
				{
					"type": "shadowsocks",
					"tag": "ss-in",
					"listen": "127.0.0.1",
					"listen_port": 8480,
					"method": "chacha20-ietf-poly1305",
					"password": "8JCsPssfgS8tiRwiMlhARg==",
					"network": "tcp"
				}
			]
		}`,
	)

	surl, _ := url.Parse(server.URL)
	transportConfig := map[string]any{
		"remote_addr":          surl.Hostname(),
		"remote_port":          surl.Port(),
		"password":             "8JCsPssfgS8tiRwiMlhARg==",
		"method":               "chacha20-ietf-poly1305",
		"internal_buffer_size": 16383,
	}
	tmp := t.TempDir()
	options := option.WATEROutboundOptions{
		DownloadTimeout: "10s",
		Dir:             tmp,
		WASMAvailableAt: []string{server.URL + "/shadowsocks_client.wasm"},
		Transport:       "shadowsocks",
		ServerOptions:   O.ServerOptions{Server: "127.0.0.1", ServerPort: 8480},
		DialerOptions:   O.DialerOptions{},
		Config:          transportConfig,
		SkipHandshake:   true,
		Hashsum:         "05b820148199ea64a7f3e716adefb4c247c8ea0f69175a3af61c36260773e273",
	}

	out, err := NewOutbound(ctx, nil, log.NewNOPFactory().Logger(), "test", options)
	if err != nil {
		t.Fatalf("failed to create outbound: %v", err)
	}

	port, _ := strconv.Atoi(surl.Port())
	var tests = []struct {
		name            string
		givenDomain     string
		givenHost       string
		givenRemoteAddr string
		givenRemotePort uint16
	}{
		{
			name:            "local request should succeed",
			givenDomain:     "http://lantern.io",
			givenHost:       surl.Host,
			givenRemoteAddr: surl.Hostname(),
			givenRemotePort: uint16(port),
		},
		{
			name:            "external request to google.com should succeed",
			givenDomain:     "https://google.com",
			givenHost:       "google.com",
			givenRemoteAddr: "172.217.29.78",
			givenRemotePort: 443,
		},
		{
			name:            "external request to ifconfig.me should succeed",
			givenDomain:     "https://ifconfig.me/ip",
			givenHost:       "ifconfig.me",
			givenRemoteAddr: "34.160.111.145",
			givenRemotePort: 443,
		},
		{
			name:            "external request to startpage.com should succeed",
			givenDomain:     "https://www.startpage.com",
			givenHost:       "www.startpage.com",
			givenRemoteAddr: "67.63.51.231",
			givenRemotePort: 443,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", tt.givenDomain, http.NoBody)
			if err != nil {
				t.Fatalf("failed to create HTTP request: %v", err)
			}
			req.Header.Set("Host", tt.givenHost)
			req.Header.Set("Accept", "*/*")

			client := &http.Client{
				Transport: &http.Transport{
					DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
						return out.DialContext(ctx, network, metadata.ParseSocksaddrHostPort(tt.givenRemoteAddr, tt.givenRemotePort))
					},
				},
				Timeout: 10 * time.Second,
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("failed to do request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected status code 200, got %d", resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err, "failed to read response body")
			// t.Logf("response: %s", body)
			t.Logf("len body: %d", len(body))
		})
	}
}

func startTestHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shadowsocks_client.wasm" {
			w.Header().Set("Content-Type", "application/wasm")
			data, err := os.ReadFile("testdata/shadowsocks_client.wasm")
			if err != nil {
				http.Error(w, "file not found", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Success!"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func startBoxServer(t *testing.T, opts string) (*box.Box, context.Context) {
	t.Helper()
	ctx := box.Context(context.Background(), include.InboundRegistry(), include.OutboundRegistry(), include.EndpointRegistry(), include.DNSTransportRegistry(), include.ServiceRegistry())

	options, err := json.UnmarshalExtendedContext[O.Options](ctx, []byte(opts))
	require.NoError(t, err, "failed to unmarshal options")

	b, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	require.NoError(t, err, "failed to create box instance")
	require.NoError(t, b.Start(), "failed to start box instance")
	t.Cleanup(func() { b.Close() })
	return b, ctx
}
