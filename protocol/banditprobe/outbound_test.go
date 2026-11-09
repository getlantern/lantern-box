package banditprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/include"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// callbackHost is a name the system resolver cannot answer, so a callback
// that reaches the API proves its host was resolved by sing-box.
const callbackHost = "api.banditprobe.test"

// recordingDialer dials the test server whatever destination it is asked for,
// and records what that was.
var _ N.Dialer = (*recordingDialer)(nil)

type recordingDialer struct {
	target string
	mu     sync.Mutex
	dialed []M.Socksaddr
}

func (d *recordingDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, destination)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func (d *recordingDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("unexpected ListenPacket")
}

func (d *recordingDialer) destinations() []M.Socksaddr {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]M.Socksaddr(nil), d.dialed...)
}

// trustServer makes client trust srv's certificate, which is issued for
// example.com, while keeping verification on.
func trustServer(t *testing.T, client *http.Client, srv *httptest.Server) {
	t.Helper()
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "the callback client must use an *http.Transport")
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "example.com"}
}

func TestCallbackClient_DialsThroughSingBoxDialer(t *testing.T) {
	rec := newCallbackRecorder(t)
	_, port, err := net.SplitHostPort(rec.srv.Listener.Addr().String())
	require.NoError(t, err)
	dialer := &recordingDialer{target: rec.srv.Listener.Addr().String()}

	cfg, err := newConfig(option.BanditProbeOutboundOptions{CallbackURL: "https://" + net.JoinHostPort(callbackHost, port) + "/v1/bandit/callback"})
	require.NoError(t, err)
	client := newCallbackClient(dialer, nil)
	trustServer(t, client, rec.srv)
	r := &responder{cfg: cfg, httpClient: client, now: time.Now}

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
	dialed := dialer.destinations()
	require.NotEmpty(t, dialed)
	assert.Equal(t, callbackHost, dialed[0].Fqdn, "the callback host must reach the sing-box dialer unresolved")
	assert.Equal(t, port, fmt.Sprint(dialed[0].Port))
}

func TestCallbackClient_IgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	rec := newCallbackRecorder(t)
	dialer := &recordingDialer{target: rec.srv.Listener.Addr().String()}

	opts := option.BanditProbeOutboundOptions{CallbackURL: "https://" + callbackHost + "/v1/bandit/callback"}
	cfg, err := newConfig(opts)
	require.NoError(t, err)
	client := newCallbackClient(dialer, callbackProxy(opts))
	trustServer(t, client, rec.srv)
	r := &responder{cfg: cfg, httpClient: client, now: time.Now}

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
	assert.Equal(t, callbackHost, dialer.destinations()[0].Fqdn)
}

func TestCallbackProxy_UseProxyEnvironment(t *testing.T) {
	assert.NotNil(t, callbackProxy(option.BanditProbeOutboundOptions{UseProxyEnvironment: true}))
}

// connectProxy answers CONNECT requests by tunnelling to target, whatever
// host was asked for, and records the requested hosts.
type connectProxy struct {
	target string
	mu     sync.Mutex
	hosts  []string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.mu.Unlock()
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func (p *connectProxy) requested() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

func TestCallbackClient_ThroughProxyDialsProxyThroughSingBoxDialer(t *testing.T) {
	rec := newCallbackRecorder(t)
	proxy := &connectProxy{target: rec.srv.Listener.Addr().String()}
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	proxyURL, err := url.Parse(proxySrv.URL)
	require.NoError(t, err)
	dialer := &recordingDialer{target: proxySrv.Listener.Addr().String()}

	cfg, err := newConfig(option.BanditProbeOutboundOptions{CallbackURL: "https://" + callbackHost + "/v1/bandit/callback"})
	require.NoError(t, err)
	client := newCallbackClient(dialer, http.ProxyURL(proxyURL))
	trustServer(t, client, rec.srv)
	r := &responder{cfg: cfg, httpClient: client, now: time.Now}

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
	assert.Equal(t, []string{net.JoinHostPort(callbackHost, "443")}, proxy.requested(), "the proxy must be asked for the callback host, unresolved")
	dialed := dialer.destinations()
	require.NotEmpty(t, dialed)
	assert.Equal(t, proxyURL.Host, dialed[0].String(), "the proxy itself must be dialed through the sing-box dialer")
}

// newBoxContext is a sing-box context with the banditprobe outbound
// registered. The lantern-box root package can't be imported here: it imports
// this package.
func newBoxContext(t *testing.T) context.Context {
	t.Helper()
	ctx := include.Context(context.Background())
	reg, ok := common.Cast[*outbound.Registry](service.FromContext[adapter.OutboundRegistry](ctx))
	require.True(t, ok)
	RegisterOutbound(reg)
	return ctx
}

func startBox(t *testing.T, config string) *sbox.Box {
	t.Helper()
	ctx := newBoxContext(t)
	opts, err := json.UnmarshalExtendedContext[sbOption.Options](ctx, []byte(config))
	require.NoError(t, err)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	require.NoError(t, b.Start())
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func boxResponder(t *testing.T, b *sbox.Box) *responder {
	t.Helper()
	out, ok := b.Outbound().Outbound("bandit-probe")
	require.True(t, ok)
	o, ok := out.(*Outbound)
	require.True(t, ok)
	return o.responder
}

// A launch config with no dns section and no dialer fields, as lantern-cloud
// generates for most VPS proxies, must still build the callback dialer and
// resolve the callback host, through sing-box's fallback local DNS server.
func TestNewOutbound_MinimalConfigWithoutDNS(t *testing.T) {
	rec := newCallbackRecorder(t)
	_, port, err := net.SplitHostPort(rec.srv.Listener.Addr().String())
	require.NoError(t, err)

	b := startBox(t, `{"log":{"disabled":true},
	"outbounds":[{"type":"direct","tag":"direct"},
	{"type":"banditprobe","tag":"bandit-probe","callback_url":"https://`+net.JoinHostPort("localhost", port)+`/v1/bandit/callback"}]}`)
	r := boxResponder(t, b)
	trustServer(t, r.httpClient, rec.srv)

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
}

// With use_proxy_environment, the sing-box dialer resolves a proxy named by
// hostname even when callback_url is an IP literal.
func TestNewOutbound_ProxyHostnameWithIPCallback(t *testing.T) {
	rec := newCallbackRecorder(t)
	_, port, err := net.SplitHostPort(rec.srv.Listener.Addr().String())
	require.NoError(t, err)
	proxy := &connectProxy{target: rec.srv.Listener.Addr().String()}
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	_, proxyPort, err := net.SplitHostPort(proxySrv.Listener.Addr().String())
	require.NoError(t, err)

	b := startBox(t, `{"log":{"disabled":true},
	"outbounds":[{"type":"direct","tag":"direct"},
	{"type":"banditprobe","tag":"bandit-probe","use_proxy_environment":true,"callback_url":"https://`+net.JoinHostPort("127.0.0.1", port)+`/v1/bandit/callback"}]}`)
	r := boxResponder(t, b)
	trustServer(t, r.httpClient, rec.srv)
	// http.ProxyFromEnvironment reads the environment once per process, so
	// the test names the proxy directly.
	transport, ok := r.httpClient.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, transport.Proxy, "use_proxy_environment must set a proxy function")
	transport.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", proxyPort)})

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
	assert.Equal(t, []string{net.JoinHostPort("127.0.0.1", port)}, proxy.requested())
}

// The callback host is resolved by sing-box's DNS router, not the system
// resolver: only the box's hosts server knows callbackHost.
func TestNewOutbound_ResolvesCallbackThroughSingBoxDNS(t *testing.T) {
	rec := newCallbackRecorder(t)
	_, port, err := net.SplitHostPort(rec.srv.Listener.Addr().String())
	require.NoError(t, err)

	b := startBox(t, `{"log":{"disabled":true},
	"dns":{"servers":[{"type":"hosts","tag":"hosts","predefined":{"`+callbackHost+`":["127.0.0.1"]}}]},
	"outbounds":[{"type":"direct","tag":"direct"},
	{"type":"banditprobe","tag":"bandit-probe","callback_url":"https://`+net.JoinHostPort(callbackHost, port)+`/v1/bandit/callback"}]}`)
	r := boxResponder(t, b)
	trustServer(t, r.httpClient, rec.srv)

	require.NoError(t, r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered}))
	require.Len(t, rec.calls(), 1)
}
