package clientcontext

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
)

const testOptionsPath = "../../testdata/options"

var testClientInfo = ClientInfo{
	DeviceID:    "lantern-box",
	Platform:    "linux",
	IsPro:       false,
	CountryCode: "US",
	Version:     "9.0",
}

// newTestInjector returns an Injector that sends testClientInfo through the
// outbounds tagged tags.
func newTestInjector(tags ...string) *Injector {
	return NewInjector(func() ClientInfo { return testClientInfo }, tags...)
}

// The router path injects only when the outbound that carries the flow, or the
// group member it selects, is enabled.
func TestMatch(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		disabled bool
		selector string
		want     bool
	}{
		{name: "no injector", disabled: true},
		{name: "match", tags: []string{"http-out"}, want: true},
		{name: "no match", tags: []string{"not-exist"}},
		{name: "group match", tags: []string{"socks-out"}, selector: "socks-out", want: true},
		{name: "group no match", tags: []string{"socks-out"}, selector: "http-out"},
	}

	srv := startTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv.tracker.reset()
			var injector *Injector
			if !tt.disabled {
				injector = newTestInjector(tt.tags...)
			}
			client := srv.startClient(t, injector, func(opts *option.Options) {
				if tt.selector != "" {
					setSelectorDefaultTag(opts, tt.selector)
				}
			})
			client.get(t)
			if tt.want {
				assert.Equal(t, &testClientInfo, srv.tracker.info())
			} else {
				assert.Nil(t, srv.tracker.info())
			}
		})
	}
}

func TestTagChanges(t *testing.T) {
	srv := startTestServer(t)
	injector := newTestInjector()
	client := srv.startClient(t, injector, nil)

	client.get(t)
	assert.Nil(t, srv.tracker.info())

	injector.AddOutboundTags("http-out")
	client.get(t)
	assert.Equal(t, &testClientInfo, srv.tracker.info())

	srv.tracker.reset()
	injector.RemoveOutboundTags("http-out")
	client.get(t)
	assert.Nil(t, srv.tracker.info())
}

type testServer struct {
	port     uint16
	upstream string
	tracker  *mockTracker
}

func startTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := box.BaseContext()
	port := freePort(t)
	serverOpts := getOptions(ctx, t, testOptionsPath+"/http_server.json")
	serverOpts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = port
	return &testServer{
		port:     port,
		upstream: startHTTPServer(t).URL,
		tracker:  startServerBox(t, ctx, serverOpts),
	}
}

// startServerBox starts a server box with a Manager and, after it, a
// mockTracker recording the client info the Manager decodes.
func startServerBox(t *testing.T, ctx context.Context, opts option.Options) *mockTracker {
	t.Helper()
	serverBox, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	tracker := &mockTracker{}
	serverBox.Router().AppendTracker(NewManager(MatchBounds{[]string{"any"}, []string{"any"}}, log.NewNOPFactory().NewLogger("")))
	serverBox.Router().AppendTracker(tracker)
	require.NoError(t, serverBox.Start())
	t.Cleanup(func() { serverBox.Close() })
	return tracker
}

type testClient struct {
	box       *sbox.Box
	proxy     *http.Client
	proxyAddr string
	upstream  string
}

// startClient starts a client box routing to the server through http-out, with
// injector installed when it is non-nil.
func (s *testServer) startClient(t *testing.T, injector *Injector, patch func(*option.Options)) *testClient {
	t.Helper()
	ctx := box.BaseContext()
	clientPort := freePort(t)
	opts := getOptions(ctx, t, testOptionsPath+"/http_client.json")
	opts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = clientPort
	for _, ob := range opts.Outbounds {
		switch o := ob.Options.(type) {
		case *option.HTTPOutboundOptions:
			o.ServerPort = s.port
		case *option.SOCKSOutboundOptions:
			o.ServerPort = s.port
		}
	}
	if patch != nil {
		patch(&opts)
	}
	if injector != nil {
		require.NoError(t, injector.Install(ctx))
	}
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	t.Cleanup(func() { instance.Close() })

	return newTestClient(t, instance, getProxyAddress(opts.Inbounds), s.upstream)
}

// newTestClient returns a testClient that requests upstream through the HTTP
// proxy inbound of instance at proxyAddr.
func newTestClient(t *testing.T, instance *sbox.Box, proxyAddr, upstream string) *testClient {
	t.Helper()
	proxyURL, err := url.Parse("http://" + proxyAddr)
	require.NoError(t, err)
	return &testClient{
		box:       instance,
		proxy:     &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}},
		proxyAddr: proxyAddr,
		upstream:  upstream,
	}
}

// get requests the upstream through the client's HTTP inbound and router.
func (c *testClient) get(t *testing.T) {
	t.Helper()
	resp, err := c.proxy.Get(c.upstream)
	require.NoError(t, err)
	requireUpstreamResponse(t, resp)
}

// dial dials the upstream directly through the outbound tagged tag, bypassing
// the router.
func (c *testClient) dial(t *testing.T, ctx context.Context, network, tag string) net.Conn {
	t.Helper()
	out, ok := c.box.Outbound().Outbound(tag)
	require.True(t, ok)
	upstream, err := url.Parse(c.upstream)
	require.NoError(t, err)
	conn, err := out.DialContext(ctx, network, M.ParseSocksaddr(upstream.Host))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// getOver requests the upstream over conn.
func (c *testClient) getOver(t *testing.T, conn net.Conn) {
	t.Helper()
	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil },
	}}
	defer httpClient.CloseIdleConnections()
	resp, err := httpClient.Get(c.upstream)
	require.NoError(t, err)
	requireUpstreamResponse(t, resp)
}

// requireUpstreamResponse asserts the upstream's response reached the client
// byte-intact: a client-info frame leaking into the stream would corrupt it.
func requireUpstreamResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, upstreamResponseBody, string(body))
}

func loopbackListen(port uint16) option.ListenOptions {
	return option.ListenOptions{
		Listen:     common.Ptr(badoption.Addr(netip.MustParseAddr("127.0.0.1"))),
		ListenPort: port,
	}
}

// startProxyPair starts a server box with inbound and a Manager, and a client
// box whose HTTP inbound routes through outbound with injection enabled for its
// tag. newInbound and newOutbound receive the server's port, and patch, if set,
// adjusts the client's options.
func startProxyPair(t *testing.T, newInbound func(port uint16) option.Inbound, newOutbound func(port uint16) option.Outbound, patch ...func(*option.Options)) (*mockTracker, *testClient) {
	t.Helper()
	serverPort := freePort(t)
	tracker := startServerBox(t, box.BaseContext(), option.Options{
		Log:      &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{newInbound(serverPort)},
	})

	clientCtx := box.BaseContext()
	clientPort := freePort(t)
	out := newOutbound(serverPort)
	require.NoError(t, newTestInjector(out.Tag).Install(clientCtx))
	clientOpts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{{
			Type:    constant.TypeHTTP,
			Tag:     "http-client",
			Options: &option.HTTPMixedInboundOptions{ListenOptions: loopbackListen(clientPort)},
		}},
		Outbounds: []option.Outbound{out},
	}
	for _, p := range patch {
		p(&clientOpts)
	}
	clientBox, err := sbox.New(sbox.Options{Context: clientCtx, Options: clientOpts})
	require.NoError(t, err)
	require.NoError(t, clientBox.Start())
	t.Cleanup(func() { clientBox.Close() })

	return tracker, newTestClient(t, clientBox, fmt.Sprintf("127.0.0.1:%d", clientPort), startHTTPServer(t).URL)
}

// startUDPEcho starts a UDP server that echoes every datagram.
func startUDPEcho(t *testing.T) M.Socksaddr {
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
			conn.WriteTo(buf[:n], addr)
		}
	}()
	return M.ParseSocksaddr(conn.LocalAddr().String())
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

// upstreamResponseBody is returned by the test upstream so tests can assert the
// proxied response arrives byte-intact.
const upstreamResponseBody = "hello from the upstream origin"

func startHTTPServer(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamResponseBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return uint16(port)
}

var _ (adapter.ConnectionTracker) = (*mockTracker)(nil)

type mockTracker struct {
	last atomic.Pointer[ClientInfo]
}

func (t *mockTracker) info() *ClientInfo { return t.last.Load() }

func (t *mockTracker) reset() { t.last.Store(nil) }

func (t *mockTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	if info, ok := InfoFromConn(conn); ok {
		t.last.Store(&info)
	}
	return conn
}

func (t *mockTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	if info, ok := InfoFromConn(conn); ok {
		t.last.Store(&info)
	}
	return conn
}
