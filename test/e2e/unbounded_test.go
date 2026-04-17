package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armon/go-socks5"
	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/getlantern/broflake/egress"
	"github.com/getlantern/broflake/freddie"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lbAdapter "github.com/getlantern/lantern-box/adapter"
	lbOption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol"
)

// TestUnboundedE2E brings up the full Unbounded stack in-process (freddie
// signaling server, egress SOCKS5+QUIC-over-WebSocket server, a widget
// producer, and a sing-box consumer with our outbound) and verifies that a
// plain HTTP request made through the sing-box proxy traverses the entire
// chain and reaches a local upstream.
//
// Chain:
//
//	test HTTP client → sing-box mixed inbound (HTTP proxy)
//	                 → unbounded outbound
//	                 → broflake consumer (this test process)
//	                 → WebRTC data channel
//	                 → broflake widget (this test process)
//	                 → WebSocket over QUIC
//	                 → egress SOCKS5 server (this test process)
//	                 → local net.Dial
//	                 → upstream httptest.Server
//
// All five services run in the same process. Nothing touches real networks
// beyond loopback. Requires WebRTC to work on the test machine (STUN is
// configured with a public server but for host-candidate-only localhost
// pairings that's usually unnecessary).
func TestUnboundedE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e (requires live WebRTC bootstrap)")
	}

	// Keep broflake's internal chatter off stderr for cleaner test output.
	UBCommon.SetDebugLogger(stdlog.New(io.Discard, "", 0))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// 1. Upstream target the proxied request lands at.
	// Atomic counter because httptest serves handlers on its own goroutine;
	// a plain int would race the test goroutine's final assertion under -race.
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}))
	t.Cleanup(upstream.Close)

	// 2. Freddie.
	freddieAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	f, err := freddie.New(ctx, freddieAddr)
	require.NoError(t, err, "freddie.New")
	go func() { _ = f.ListenAndServe() }()
	freddieURL := "http://" + freddieAddr
	waitForHTTP(t, freddieURL+"/v1/", 5*time.Second)

	// 3. Egress — a plain SOCKS5 server whose listener is wrapped in the
	//    broflake egress layer (QUIC over WebSocket). We use an insecure TLS
	//    config because the widget-side peer presents a self-signed QUIC cert
	//    that the consumer validates via the InsecureDoNotVerify path.
	egL, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen for egress")
	egressWrapped, err := egress.NewListener(ctx, egL, &tls.Config{
		NextProtos:         []string{"broflake"},
		InsecureSkipVerify: true,
	})
	require.NoError(t, err, "egress.NewListener")
	t.Cleanup(func() { _ = egressWrapped.Close() })

	socksServer, err := socks5.New(&socks5.Config{})
	require.NoError(t, err, "socks5.New")
	go func() { _ = socksServer.Serve(egressWrapped) }()
	egressURL := "ws://" + egL.Addr().String()

	// 4. Widget (producer) — constructed exactly like broflake's cmd/desktop
	//    with ClientType="widget". Runs entirely in this process; no
	//    subprocess, no wasm.
	startWidget(t, ctx, freddieURL, egressURL)

	// 5. Consumer — sing-box with our Unbounded outbound + a mixed HTTP inbound.
	proxyAddr := startConsumer(t, ctx, freddieURL, egressURL)

	// 6. Proxied HTTP request. Give the whole chain a generous deadline —
	//    freddie matchmaking + ICE + DTLS + QUIC handshake + SOCKS5 round-trip
	//    all need to complete on the first shot.
	proxyURL, _ := url.Parse("http://" + proxyAddr)
	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   60 * time.Second,
	}

	var lastErr error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(upstream.URL)
		if err == nil {
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "ok", resp.Header.Get("X-Upstream"))
			require.GreaterOrEqual(t, upstreamHits.Load(), int64(1), "upstream should have been hit at least once")
			return
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("proxy request never succeeded within 60s. last error: %v", lastErr)
}

// startWidget spins up a broflake widget (producer) in this process. It
// connects to freddie as a volunteer and waits to be paired with a consumer.
// Returns when the widget has constructed successfully; pairing happens
// asynchronously once the consumer comes online.
func startWidget(t *testing.T, ctx context.Context, freddieURL, egressURL string) {
	t.Helper()

	bfOpt := UBClientcore.NewDefaultBroflakeOptions()
	bfOpt.ClientType = "widget"
	bfOpt.CTableSize = 2
	bfOpt.PTableSize = 2

	rtcOpt := UBClientcore.NewDefaultWebRTCOptions()
	rtcOpt.DiscoverySrv = freddieURL
	rtcOpt.STUNBatch = func(size uint32) ([]string, error) {
		// Host candidates suffice for loopback pairing; return the public
		// Google STUN only in case the libraries require a non-empty batch.
		return []string{"stun:stun.l.google.com:19302"}, nil
	}

	egOpt := UBClientcore.NewDefaultEgressOptions()
	egOpt.Addr = egressURL

	_, ui, err := UBClientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
	require.NoError(t, err, "widget NewBroflake")
	t.Cleanup(ui.Stop)

	// Give the widget a moment to register with freddie before the consumer
	// starts asking for peers. Without this, the first consumer poll can race
	// the widget's registration and the test fails with a "no peers" error
	// that requires the retry loop above to recover from.
	time.Sleep(500 * time.Millisecond)
}

// startConsumer builds a sing-box instance containing a mixed HTTP/SOCKS
// inbound and our Unbounded outbound, starts it, and returns the inbound's
// listen address.
func startConsumer(t *testing.T, ctx context.Context, freddieURL, egressURL string) string {
	t.Helper()

	boxCtx := box.BaseContext()
	// Registers lantern-box's custom protocols (including unbounded) against
	// the sing-box inbound/outbound/endpoint registries on boxCtx.
	boxCtx = protocol.RegisterProtocols(boxCtx)

	// No direct transport registered: the consumer has no VPN tunnel in this
	// test, so signaling falls back to the outbound's own dialer. Fine for
	// this test — we just want to exercise the code path end-to-end.
	_ = lbAdapter.DirectTransportFromContext(boxCtx)

	inboundPort := freePort(t)
	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))

	unboundedOpts := lbOption.UnboundedOutboundOptions{
		InsecureDoNotVerifyClientCert: true,
		DiscoverySrv:                  freddieURL,
		EgressAddr:                    egressURL,
		STUNServers:                   []string{"stun:stun.l.google.com:19302"},
		STUNBatchSize:                 1,
		// ConsumerSessionID is arbitrary — any unique value works.
		ConsumerSessionID: "e2e-consumer",
	}

	opts := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Inbounds: []option.Inbound{
			{
				Type: "mixed",
				Tag:  "http-in",
				Options: &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     &listenAddr,
						ListenPort: inboundPort,
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type:    "unbounded",
				Tag:     "unbounded-out",
				Options: &unboundedOpts,
			},
		},
		Route: &option.RouteOptions{Final: "unbounded-out"},
	}

	b, err := sbox.New(sbox.Options{
		Context: boxCtx,
		Options: opts,
	})
	require.NoError(t, err, "sbox.New")
	require.NoError(t, b.Start(), "sbox Start")
	t.Cleanup(func() { _ = b.Close() })

	return fmt.Sprintf("127.0.0.1:%d", inboundPort)
}

// waitForHTTP polls the given URL until it returns any response (even an
// error status) or the deadline passes. Used as a readiness gate for freddie.
func waitForHTTP(t *testing.T, target string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(target)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became ready within %s", target, timeout)
}
