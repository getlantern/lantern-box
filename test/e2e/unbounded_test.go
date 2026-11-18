package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armon/go-socks5"
	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/getlantern/broflake/common/helperticket"
	"github.com/getlantern/broflake/egress"
	"github.com/getlantern/broflake/freddie"
	sbox "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
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
	// Shutdown waits on f's context for in-flight long polls; cancel it
	// first so the listener closes without that wait.
	t.Cleanup(func() { cancel(); _ = f.Shutdown() })
	go func() { _ = f.ListenAndServe() }()
	freddieURL := "http://" + freddieAddr
	waitForHTTP(t, freddieURL+"/v1/", 5*time.Second)

	// 3. Egress: a plain SOCKS5 server behind the broflake egress layer.
	egressURL := startEgress(t, ctx)

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
//
// Each configure func adjusts the widget's options before it starts.
func startWidget(t *testing.T, ctx context.Context, freddieURL, egressURL string,
	configure ...func(*UBClientcore.BroflakeOptions, *UBClientcore.WebRTCOptions)) {
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

	for _, c := range configure {
		c(bfOpt, rtcOpt)
	}

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
// listen address. With roomTickets, the outbound is tagged like a paired
// inviter's helper consumer and presents them to join its pairings' rooms.
func startConsumer(t *testing.T, ctx context.Context, freddieURL, egressURL string, roomTickets ...string) string {
	t.Helper()
	tag := "unbounded-out"
	if len(roomTickets) > 0 {
		tag = e2eHelperTag
	}
	return startConsumers(t, ctx, freddieURL, egressURL, []string{tag}, roomTickets...)[tag]
}

// startConsumers builds one sing-box instance with an Unbounded outbound for
// each of tags, each reached through its own mixed inbound, starts it, and
// returns each outbound's inbound listen address by tag. All the outbounds
// share the box context, so with roomTickets they all see the same ticket
// source, as they do in radiance; which of them use it is the outbound's
// call.
func startConsumers(t *testing.T, ctx context.Context, freddieURL, egressURL string, tags []string, roomTickets ...string) map[string]string {
	t.Helper()

	boxCtx := box.BaseContext()
	// Registers lantern-box's custom protocols (including unbounded) against
	// the sing-box inbound/outbound/endpoint registries on boxCtx.
	boxCtx = protocol.RegisterProtocols(boxCtx)

	// No direct transport registered: the consumer has no VPN tunnel in this
	// test, so signaling falls back to the outbound's own dialer.
	if len(roomTickets) > 0 {
		tickets := slices.Clone(roomTickets)
		boxCtx = lbAdapter.ContextWithRoomTicketSource(boxCtx, func() []string { return tickets })
	}

	listenAddr := badoption.Addr(netip.MustParseAddr("127.0.0.1"))
	opts := option.Options{
		Log:   &option.LogOptions{Disabled: true},
		Route: &option.RouteOptions{Final: tags[0]},
	}
	addrs := make(map[string]string, len(tags))
	for _, tag := range tags {
		inboundPort := freePort(t)
		inTag := "in-" + tag
		opts.Inbounds = append(opts.Inbounds, option.Inbound{
			Type: "mixed",
			Tag:  inTag,
			Options: &option.HTTPMixedInboundOptions{
				ListenOptions: option.ListenOptions{
					Listen:     &listenAddr,
					ListenPort: inboundPort,
				},
			},
		})
		opts.Outbounds = append(opts.Outbounds, option.Outbound{
			Type: "unbounded",
			Tag:  tag,
			Options: &lbOption.UnboundedOutboundOptions{
				InsecureDoNotVerifyClientCert: true,
				DiscoverySrv:                  freddieURL,
				EgressAddr:                    egressURL,
				STUNServers:                   []string{"stun:stun.l.google.com:19302"},
				STUNBatchSize:                 1,
				ConsumerSessionID:             fmt.Sprintf("e2e-consumer-%d", consumerSeq.Add(1)),
			},
		})
		opts.Route.Rules = append(opts.Route.Rules, option.Rule{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{
				RawDefaultRule: option.RawDefaultRule{Inbound: badoption.Listable[string]{inTag}},
				RuleAction: option.RuleAction{
					Action:       C.RuleActionTypeRoute,
					RouteOptions: option.RouteActionOptions{Outbound: tag},
				},
			},
		})
		addrs[tag] = fmt.Sprintf("127.0.0.1:%d", inboundPort)
	}

	b, err := sbox.New(sbox.Options{
		Context: boxCtx,
		Options: opts,
	})
	require.NoError(t, err, "sbox.New")
	require.NoError(t, b.Start(), "sbox Start")
	t.Cleanup(func() { _ = b.Close() })

	return addrs
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

// consumerSeq numbers consumers so that each has its own session ID.
var consumerSeq atomic.Int64

const (
	// e2eHelperTag is shaped like the tag lantern-cloud gives a paired
	// inviter's dedicated Unbounded consumer: "helper-" + 12 lowercase hex.
	e2eHelperTag = "helper-5e2f0a9c41d7"
	// e2ePoolTag is shaped like a bandit-assigned pool consumer's tag.
	e2ePoolTag = "unbounded-out-pool"
)

// TestUnboundedRoomE2E checks helper pairing rooms end to end. A volunteer
// widget that holds a pairing's volunteer ticket and serves only its rooms
// serves the inviter's helper consumer, which holds the same pairing's
// inviter ticket. freddie never offers it to a consumer outside the room:
// not to one with no tickets, even while that consumer is the only one
// waiting, and not to the pool consumer in the inviter's own box, which sees
// the same ticket source but must not use it (only helper- outbounds join
// rooms).
//
// freddie keeps its tables in package globals shared by every freddie in the
// process, so this test must not overlap another Unbounded test, and neither
// Unbounded test may call t.Parallel(). Go runs them one after another, and
// TestUnboundedE2E's cleanup stops its pool widget and its freddie.
func TestUnboundedRoomE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e (requires live WebRTC bootstrap)")
	}
	UBCommon.SetDebugLogger(stdlog.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// This test's stand-in for lantern-cloud's ticket key. Tickets are
	// credentials: never log them, even here.
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	keys := []helperticket.Key{helperticket.NewKey(priv.Public().(ed25519.PublicKey))}
	const pairingID = "6f1c2b0e-8a7d-4c1e-9f00-0123456789ab"
	ticket := func(role helperticket.Role) string {
		tk, err := helperticket.Sign(priv, helperticket.Ticket{PairingID: pairingID, Role: role, Expires: time.Now().Add(15 * time.Minute)})
		require.NoError(t, err)
		return tk
	}

	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}))
	t.Cleanup(upstream.Close)

	freddieAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	f, err := freddie.New(ctx, freddieAddr)
	require.NoError(t, err, "freddie.New")
	// Shutdown waits on f's context for in-flight long polls; cancel it
	// first so the listener closes without that wait.
	t.Cleanup(func() { cancel(); _ = f.Shutdown() })
	f.RoomTicket = helperticket.RoomFunc(func() []helperticket.Key { return keys }, time.Now)
	go func() { _ = f.ListenAndServe() }()
	freddieURL := "http://" + freddieAddr
	waitForHTTP(t, freddieURL+"/v1/", 5*time.Second)

	egressURL := startEgress(t, ctx)

	// The volunteer's connection slots (workers) that a consumer ever
	// connected to over WebRTC. Only the helper consumer may ever connect, so
	// a room leak fails the test here even if the leaked connection could not
	// have carried traffic.
	var (
		accepted     atomic.Int64
		connectedMu  sync.Mutex
		connectedIdx = map[int]bool{}
	)
	startWidget(t, ctx, freddieURL, egressURL, func(bfOpt *UBClientcore.BroflakeOptions, rtcOpt *UBClientcore.WebRTCOptions) {
		bfOpt.OnConnectionChangeFunc = func(state int, workerIdx int, _ net.IP) {
			if state == 1 {
				accepted.Add(1)
				connectedMu.Lock()
				connectedIdx[workerIdx] = true
				connectedMu.Unlock()
			}
		}
		rtcOpt.SetRoomTickets([]string{ticket(helperticket.RoleVolunteer)})
		rtcOpt.SetServePool(false)
	})

	// Alone with a volunteer that served the pool, this consumer would be
	// matched at once. This volunteer serves only its room.
	unpaired := startConsumer(t, ctx, freddieURL, egressURL)
	requireNeverProxies(t, unpaired, upstream.URL, &upstreamHits, 15*time.Second)
	require.Zero(t, accepted.Load(), "the pairings-only volunteer accepted a consumer outside its room")

	// The inviter's box: her helper consumer and a pool consumer side by
	// side, both with her tickets on the box context.
	inviter := startConsumers(t, ctx, freddieURL, egressURL,
		[]string{e2eHelperTag, e2ePoolTag}, ticket(helperticket.RoleInviter))
	requireProxies(t, inviter[e2eHelperTag], upstream.URL, 60*time.Second)
	require.GreaterOrEqual(t, accepted.Load(), int64(1), "the volunteer saw the helper consumer connect")

	// The volunteer now has a free slot, and the inviter's pool consumer
	// never gets it. (The unpaired consumer is still waiting too, so this
	// window covers it with a free slot as well.)
	requireNeverProxies(t, inviter[e2ePoolTag], upstream.URL, &upstreamHits, 10*time.Second)

	connectedMu.Lock()
	workers := len(connectedIdx)
	connectedMu.Unlock()
	require.Equal(t, 1, workers,
		"only the helper consumer may ever connect to the volunteer; %d of its workers saw a connection", workers)
}

// startEgress starts the broflake egress layer (QUIC over WebSocket) in front
// of a plain SOCKS5 server, and returns its ws:// URL. The TLS config is
// insecure because the widget-side peer presents a self-signed QUIC cert.
func startEgress(t *testing.T, ctx context.Context) string {
	t.Helper()
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
	return "ws://" + egL.Addr().String()
}

// proxiedGetOK reports whether a GET of target through the HTTP proxy at
// proxyAddr returned 200 within timeout.
func proxiedGetOK(proxyAddr, target string, timeout time.Duration) bool {
	proxyURL, _ := url.Parse("http://" + proxyAddr)
	c := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true},
		Timeout:   timeout,
	}
	resp, err := c.Get(target)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// requireProxies fails unless a request through proxyAddr reaches target
// within the deadline: matchmaking, ICE, DTLS, QUIC and SOCKS5 all take time.
func requireProxies(t *testing.T, proxyAddr, target string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if proxiedGetOK(proxyAddr, target, 10*time.Second) {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no request through %s succeeded within %s", proxyAddr, within)
}

// requireNeverProxies fails if any request through proxyAddr reaches target
// during d.
func requireNeverProxies(t *testing.T, proxyAddr, target string, hits *atomic.Int64, d time.Duration) {
	t.Helper()
	before := hits.Load()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		require.False(t, proxiedGetOK(proxyAddr, target, 3*time.Second),
			"a consumer outside the volunteer's room got through")
		time.Sleep(500 * time.Millisecond)
	}
	require.Equal(t, before, hits.Load(), "the upstream was reached through a consumer outside the room")
}
