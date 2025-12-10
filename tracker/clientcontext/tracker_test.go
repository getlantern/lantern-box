package clientcontext

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"testing"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

const testOptionsPath = "../../testdata/options"

func TestIntegration(t *testing.T) {
	cInfo := ClientInfo{
		DeviceID:    "sing-box-extensions",
		Platform:    "linux",
		IsPro:       false,
		CountryCode: "US",
		Version:     "9.0",
	}
	ctx := box.BoxContext()
	logger := log.NewNOPFactory().NewLogger("")
	clientTracker := NewClientContextTracker(cInfo, MatchBounds{[]string{"any"}, []string{"any"}}, logger)
	clientOpts, clientBox := newTestBox(ctx, t, testOptionsPath+"/http_client.json", clientTracker)

	httpInbound, exists := clientBox.Inbound().Get("http-client")
	require.True(t, exists, "http-client inbound should exist")
	require.Equal(t, constant.TypeHTTP, httpInbound.Type(), "http-client should be a HTTP inbound")

	// this cannot actually be empty or we would have failed to create the box instance
	proxyAddr := getProxyAddress(clientOpts.Inbounds)

	serverTracker := NewClientContextReader(MatchBounds{[]string{"any"}, []string{"any"}}, logger)
	_, serverBox := newTestBox(ctx, t, testOptionsPath+"/http_server.json", serverTracker)

	mTracker := &mockTracker{}
	serverBox.Router().AppendTracker(mTracker)

	require.NoError(t, clientBox.Start())
	defer clientBox.Close()
	require.NoError(t, serverBox.Start())
	defer serverBox.Close()

	httpServer := startHTTPServer()
	defer httpServer.Close()

	proxyURL, _ := url.Parse("http://" + proxyAddr)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	req, err := http.NewRequest("GET", httpServer.URL, nil)
	require.NoError(t, err)

	_, err = httpClient.Do(req)
	require.NoError(t, err)

	require.Equal(t, cInfo, *mTracker.info)
}

func getProxyAddress(inbounds []option.Inbound) string {
	for _, inbound := range inbounds {
		if inbound.Tag == "http-client" {
			if options, ok := inbound.Options.(*option.HTTPMixedInboundOptions); ok {
				return fmt.Sprintf("%s:%v", netip.Addr(*options.Listen).String(), options.ListenPort)
			}
		}
	}
	return ""
}

func startHTTPServer() *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(handler)
}

func newTestBox(ctx context.Context, t *testing.T, configPath string, tracker *ClientContextTracker) (option.Options, *sbox.Box) {
	buf, err := os.ReadFile(configPath)
	require.NoError(t, err)

	options, err := json.UnmarshalExtendedContext[option.Options](ctx, buf)
	require.NoError(t, err)

	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: options,
	})
	require.NoError(t, err)

	instance.Router().AppendTracker(tracker)
	return options, instance
}

var _ (adapter.ConnectionTracker) = (*mockTracker)(nil)

type mockTracker struct {
	info *ClientInfo
}

func (t *mockTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	t.info = service.PtrFromContext[ClientInfo](ctx)
	return conn
}
func (t *mockTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	return conn
}
