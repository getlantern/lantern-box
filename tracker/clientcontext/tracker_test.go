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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

const testOptionsPath = "../../testdata/options"

func TestClientContext(t *testing.T) {
	cInfo := ClientInfo{
		DeviceID:    "lantern-box",
		Platform:    "linux",
		IsPro:       false,
		CountryCode: "US",
		Version:     "9.0",
	}
	infoFn := func() ClientInfo { return cInfo }
	tests := []struct {
		name               string
		tracker            *ClientContextInjector
		useSelectorWithTag string
		shouldHaveInfo     bool
	}{
		{
			name:           "Manager without injector",
			tracker:        nil,
			shouldHaveInfo: false,
		},
		{
			name:           "Injector match",
			tracker:        NewClientContextInjector(infoFn, MatchBounds{[]string{"any"}, []string{"any"}}),
			shouldHaveInfo: true,
		},
		{
			name:           "Injector does not match",
			tracker:        NewClientContextInjector(infoFn, MatchBounds{[]string{"any"}, []string{"not-exist"}}),
			shouldHaveInfo: false,
		},
		{
			name:               "Match group real tag",
			tracker:            NewClientContextInjector(infoFn, MatchBounds{[]string{"any"}, []string{"socks-out"}}),
			useSelectorWithTag: "socks-out",
			shouldHaveInfo:     true,
		},
		{
			name:               "Does not match group real tag",
			tracker:            NewClientContextInjector(infoFn, MatchBounds{[]string{"any"}, []string{"socks-out"}}),
			useSelectorWithTag: "http-out",
			shouldHaveInfo:     false,
		},
	}

	ctx := box.BaseContext()
	logger := log.NewNOPFactory().NewLogger("")
	mgr := NewManager(MatchBounds{[]string{"any"}, []string{"any"}}, logger)
	serverOpts := getOptions(ctx, t, testOptionsPath+"/http_server.json")
	serverBox, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: serverOpts,
	})
	require.NoError(t, err)

	serverBox.Router().AppendTracker(mgr)

	require.NoError(t, serverBox.Start())
	defer serverBox.Close()

	httpServer := startHTTPServer()
	defer httpServer.Close()

	clientOpts := getOptions(ctx, t, testOptionsPath+"/http_client.json")
	proxyAddr := getProxyAddress(clientOpts.Inbounds)
	require.NotEmpty(t, proxyAddr, "http-client inbound not found in client options")

	proxyURL, _ := url.Parse("http://" + proxyAddr)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	addr := httpServer.URL

	mTracker := &mockTracker{}
	mgr.AppendTracker(mTracker)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mTracker.info = nil
			clientOpts = getOptions(ctx, t, testOptionsPath+"/http_client.json")
			if tt.useSelectorWithTag != "" {
				setSelectorDefaultTag(&clientOpts, tt.useSelectorWithTag)
			}
			runTrackerTest(ctx, t, clientOpts, tt.tracker, httpClient, addr)
			if tt.shouldHaveInfo {
				assert.Equal(t, &cInfo, mTracker.info)
			} else {
				assert.Nil(t, mTracker.info)
			}
		})
	}
}

func runTrackerTest(
	ctx context.Context,
	t *testing.T,
	opts option.Options,
	tracker *ClientContextInjector,
	client *http.Client,
	addr string,
) {
	instance, err := sbox.New(sbox.Options{
		Context: ctx,
		Options: opts,
	})
	require.NoError(t, err)
	if tracker != nil {
		instance.Router().AppendTracker(tracker)
	}

	require.NoError(t, instance.Start())
	defer instance.Close()

	req, err := http.NewRequest("GET", addr, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func getOptions(ctx context.Context, t *testing.T, configPath string) option.Options {
	buf, err := os.ReadFile(configPath)
	require.NoError(t, err)

	options, err := json.UnmarshalExtendedContext[option.Options](ctx, buf)
	require.NoError(t, err)
	return options
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

func setSelectorDefaultTag(options *option.Options, tag string) {
	for _, outbound := range options.Outbounds {
		if outbound.Type == constant.TypeSelector {
			opts := outbound.Options.(*option.SelectorOutboundOptions)
			opts.Default = tag
			break
		}
	}
	options.Route.Rules[0].DefaultOptions.RouteOptions.Outbound = "selector"
}

func startHTTPServer() *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(handler)
}

var _ (adapter.ConnectionTracker) = (*mockTracker)(nil)

type mockTracker struct {
	info *ClientInfo
}

func (t *mockTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	info, ok := ClientInfoFromContext(ctx)
	if ok {
		t.info = &info
	}
	return conn
}
func (t *mockTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	return conn
}
