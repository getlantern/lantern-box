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
		DeviceID:    "lantern-box",
		Platform:    "linux",
		IsPro:       false,
		CountryCode: "US",
		Version:     "9.0",
	}
	ctx := box.BaseContext()
	logger := log.NewNOPFactory().NewLogger("")
	serverTracker := NewClientContextReader(MatchBounds{[]string{"any"}, []string{"any"}}, logger)
	_, serverBox := newTestBox(ctx, t, testOptionsPath+"/http_server.json", serverTracker)

	mTracker := &mockTracker{}
	serverBox.Router().AppendTracker(mTracker)

	require.NoError(t, serverBox.Start())
	defer serverBox.Close()

	httpServer := startHTTPServer()
	defer httpServer.Close()

	clientOpts, clientBox := newTestBox(ctx, t, testOptionsPath+"/http_client.json", nil)

	httpInbound, exists := clientBox.Inbound().Get("http-client")
	require.True(t, exists, "http-client inbound should exist")
	require.Equal(t, constant.TypeHTTP, httpInbound.Type(), "http-client should be a HTTP inbound")

	// this cannot actually be empty or we would have failed to create the box instance
	proxyAddr := getProxyAddress(clientOpts.Inbounds)

	require.NoError(t, clientBox.Start())
	defer clientBox.Close()

	proxyURL, _ := url.Parse("http://" + proxyAddr)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	addr := httpServer.URL

	t.Run("without ClientContext tracker", func(t *testing.T) {
		req, err := http.NewRequest("GET", addr+"/ip", nil)
		require.NoError(t, err)

		_, err = httpClient.Do(req)
		require.NoError(t, err)

		require.Nil(t, mTracker.info)
		require.NotEqual(t, cInfo, mTracker.info)
	})
	t.Run("with ClientContext tracker", func(t *testing.T) {
		clientTracker := NewClientContextTracker(cInfo, MatchBounds{[]string{"any"}, []string{"any"}}, logger)
		clientBox.Router().AppendTracker(clientTracker)
		req, err := http.NewRequest("GET", addr+"/ip", nil)
		require.NoError(t, err)

		_, err = httpClient.Do(req)
		require.NoError(t, err)

		info := mTracker.info
		require.NotNil(t, info)
		require.Equal(t, cInfo, *info)
	})
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

	if tracker != nil {
		instance.Router().AppendTracker(tracker)
	}
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
