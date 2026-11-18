//go:build with_quic

package e2e

import (
	"context"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	UBCommon "github.com/getlantern/broflake/common"
	"github.com/getlantern/broflake/freddie"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lbOption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/peerrules"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// TestDirectExitE2E brings up freddie, the egress, a volunteer widget whose exit is enabled
// and routes into an exit box built from testdata/direct_exit_server.json, and a consumer
// whose unbounded outbound takes direct exits. Traffic must leave through the volunteer's
// exit box, never the egress, and the exit box's reject rules must refuse what they cover.
//
// Like the other Unbounded tests, this one must not overlap another: freddie keeps its
// tables in package globals.
func TestDirectExitE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e (requires live WebRTC bootstrap)")
	}

	var originHits atomic.Int64
	origin := httptest.NewServer(countingHandler(&originHits))
	t.Cleanup(origin.Close)
	_, originPort, err := net.SplitHostPort(origin.Listener.Addr().String())
	require.NoError(t, err)

	rig := startDirectExitRig(t, directExitCase{enabled: true, router: true, consumerDirectExit: true})

	t.Run("http via the volunteer, not the egress", func(t *testing.T) {
		requireProxies(t, rig.proxy, origin.URL, 60*time.Second)
		require.Positive(t, originHits.Load())
		require.Positive(t, rig.directSessions.Load(), "the volunteer served no direct exit session")
		require.Positive(t, rig.router.routed.Load(), "the exit box routed nothing")
		require.Zero(t, rig.egressDials.Load(), "the egress carried a direct exit's traffic")
	})

	t.Run("udp echo", func(t *testing.T) {
		echo := udpListener(t, true)

		routedBefore := rig.router.routed.Load()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client := socks.NewClient(N.SystemDialer, M.ParseSocksaddr(rig.proxy), socks.Version5, "", "")
		pc, err := client.ListenPacket(ctx, M.SocksaddrFromNet(echo.LocalAddr()))
		require.NoError(t, err, "SOCKS5 UDP associate")
		t.Cleanup(func() { _ = pc.Close() })

		// The first datagrams can go out before the flow is up end to end, so resend until
		// one comes back.
		payload := []byte("direct exit udp")
		buf := make([]byte, 2048)
		deadline := time.Now().Add(20 * time.Second)
		for {
			require.True(t, time.Now().Before(deadline), "no UDP echo through the direct exit within 20s")
			_, err := pc.WriteTo(payload, echo.LocalAddr())
			require.NoError(t, err)
			_ = pc.SetReadDeadline(time.Now().Add(time.Second))
			n, _, err := pc.ReadFrom(buf)
			if err == nil {
				require.Equal(t, payload, buf[:n])
				break
			}
		}
		require.Greater(t, rig.router.routed.Load(), routedBefore, "the exit box routed no UDP flow")
		require.Zero(t, rig.egressDials.Load(), "the egress carried a direct exit's UDP")
	})

	t.Run("udp session stays on its first destination", func(t *testing.T) {
		echo := udpListener(t, true)
		other := udpListener(t, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client := socks.NewClient(N.SystemDialer, M.ParseSocksaddr(rig.proxy), socks.Version5, "", "")
		pc, err := client.ListenPacket(ctx, M.SocksaddrFromNet(echo.LocalAddr()))
		require.NoError(t, err, "SOCKS5 UDP associate")
		t.Cleanup(func() { _ = pc.Close() })

		buf := make([]byte, 2048)
		deadline := time.Now().Add(20 * time.Second)
		for {
			require.True(t, time.Now().Before(deadline), "no UDP echo through the direct exit within 20s")
			_, err := pc.WriteTo([]byte("first"), echo.LocalAddr())
			require.NoError(t, err)
			_ = pc.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := pc.ReadFrom(buf); err == nil {
				break
			}
		}
		// The exit box's rules saw only the session's first destination: a later datagram to
		// another address must not reach it.
		for range 5 {
			_, err := pc.WriteTo([]byte("second"), other.LocalAddr())
			require.NoError(t, err)
			time.Sleep(100 * time.Millisecond)
		}
		_ = other.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, err = other.ReadFrom(buf)
		require.Error(t, err, "a later datagram reached another address than the session's first")
	})

	t.Run("10.0.0.0/8 refused", func(t *testing.T) {
		requireRefused(t, rig, "http://10.255.255.1:80/", nil)
	})

	t.Run("domain resolving to 10.0.0.0/8 refused", func(t *testing.T) {
		requireRefused(t, rig, "http://private.test:80/", nil)
	})

	t.Run("private addresses refused", func(t *testing.T) {
		// Link-local is in none of the fixture's ip_cidr rejects, only in ip_is_private.
		requireRefused(t, rig, "http://169.254.255.1:80/", nil)
		requireRefused(t, rig, "http://linklocal.test:80/", nil)
	})

	t.Run("port 25 refused", func(t *testing.T) {
		requireRefused(t, rig, "http://127.0.0.1:25/", nil)
	})

	t.Run("abuse rule set refused", func(t *testing.T) {
		// allowed.test resolves to the origin through the exit box's hosts, as the abuse
		// test domains do, so they are refused by the rule sets and not by resolution.
		requireProxies(t, rig.proxy, "http://allowed.test:"+originPort+"/", 20*time.Second)
		for _, host := range []string{"malware.test", "phishing.test", "cryptominer.test"} {
			requireRefused(t, rig, "http://"+host+":"+originPort+"/", &originHits)
		}
		// geoip-malware's inline set holds this TEST-NET-1 address.
		requireRefused(t, rig, "http://192.0.2.66:80/", nil)
	})

	t.Run("still serves after refusals", func(t *testing.T) {
		requireProxies(t, rig.proxy, origin.URL, 20*time.Second)
		require.Zero(t, rig.egressDials.Load(), "the egress carried a direct exit's traffic")
	})
}

// TestDirectExitFallsBackToEgress checks that a consumer reaches the origin through today's
// WebRTC path to the egress when the volunteer can't, or the consumer won't, take a direct
// exit.
func TestDirectExitFallsBackToEgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e (requires live WebRTC bootstrap)")
	}
	for _, tc := range []struct {
		name string
		directExitCase
	}{
		{"flag off", directExitCase{enabled: false, router: true, consumerDirectExit: true}},
		{"no registration", directExitCase{enabled: true, router: false, consumerDirectExit: true}},
		{"old consumer", directExitCase{enabled: true, router: true, consumerDirectExit: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var originHits atomic.Int64
			origin := httptest.NewServer(countingHandler(&originHits))
			t.Cleanup(origin.Close)

			rig := startDirectExitRig(t, tc.directExitCase)
			requireProxies(t, rig.proxy, origin.URL, 60*time.Second)
			require.Positive(t, originHits.Load())
			require.Positive(t, rig.egressDials.Load(), "the egress saw no dial")
			require.Zero(t, rig.directSessions.Load(), "the volunteer served a direct exit session")
			require.Zero(t, rig.router.routed.Load(), "the exit box routed traffic")
		})
	}
}

// directExitCase is what a direct exit rig sets up: whether the volunteer's exit is enabled
// and has an exit box (a registration), and whether the consumer takes direct exits. widget,
// if not nil, adjusts the widget's WebRTC options, its Exit already set, before it starts.
type directExitCase struct {
	enabled, router, consumerDirectExit bool
	widget                              func(*UBClientcore.WebRTCOptions)
}

// directExitRig is a running freddie, egress, volunteer widget with an exit, and consumer.
type directExitRig struct {
	// proxy is the consumer's mixed inbound address.
	proxy string
	// egressDials counts the egress's SOCKS5 dials out.
	egressDials *atomic.Int64
	// directSessions counts the direct exit sessions the volunteer served.
	directSessions *atomic.Int64
	// router is the exit box's router as the exit sees it, counting what it routes.
	router *countingRouter
}

func startDirectExitRig(t testing.TB, c directExitCase) *directExitRig {
	t.Helper()
	UBCommon.SetDebugLogger(stdlog.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	freddieAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	f, err := freddie.New(ctx, freddieAddr)
	require.NoError(t, err, "freddie.New")
	// Shutdown waits on f's context for in-flight long polls; cancel it first.
	t.Cleanup(func() { cancel(); _ = f.Shutdown() })
	go func() { _ = f.ListenAndServe() }()
	freddieURL := "http://" + freddieAddr
	waitForHTTP(t, freddieURL+"/v1/", 5*time.Second)

	rig := &directExitRig{
		egressDials:    new(atomic.Int64),
		directSessions: new(atomic.Int64),
		router:         &countingRouter{ConnectionRouterEx: startExitBox(t)},
	}
	egressURL := startCountingEgress(t, ctx, rig.egressDials)

	e, err := exit.New(ctx, sblog.NewNOPFactory().Logger())
	require.NoError(t, err, "exit.New")
	t.Cleanup(e.Close)
	e.SetEnabled(c.enabled)
	if c.router {
		e.SetRouter(rig.router, time.Now().Add(time.Hour))
	}
	exitOpts := *e.Options()
	onSession := exitOpts.OnSession
	exitOpts.OnSession = func(directExit bool) {
		if directExit {
			rig.directSessions.Add(1)
		}
		onSession(directExit)
	}
	startWidget(t, ctx, freddieURL, egressURL, func(_ *UBClientcore.BroflakeOptions, rtcOpt *UBClientcore.WebRTCOptions) {
		rtcOpt.Exit = &exitOpts
		if c.widget != nil {
			c.widget(rtcOpt)
		}
	})

	tag := "unbounded-out"
	rig.proxy = startConsumersWith(t, ctx, freddieURL, egressURL, []string{tag},
		func(o *lbOption.UnboundedOutboundOptions) {
			if c.consumerDirectExit {
				o.DirectExit = &lbOption.UnboundedDirectExitOptions{ServerName: "www.example.com"}
			}
		})[tag]
	return rig
}

// startExitBox checks testdata/direct_exit_server.json's rules as RunExit does and starts an
// exit box from it, in its own service registry as node's exit boxes are, and returns its
// router.
//
// The fixture is shaped as lantern-cloud's direct exit configs must be: a resolve first, the
// rejects, an ip_is_private reject and a final route with udp_connect. Only its route of
// 127.0.0.1, ahead of the ip_is_private reject, is the test's: every server here is on
// loopback, which ip_is_private refuses. That route fails the direct exit check, which allows
// no route before the ip_is_private reject, so the check runs on the fixture without it.
func startExitBox(t testing.TB) adapter.ConnectionRouterEx {
	t.Helper()
	raw, err := os.ReadFile("testdata/direct_exit_server.json")
	require.NoError(t, err)
	require.NoError(t, peerrules.ValidateAbuseRules(string(raw)), "the fixture's abuse rules")

	ctx := service.ExtendContext(box.BaseContext())
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, raw)
	require.NoError(t, err, "parse the exit box's options")
	require.Error(t, peerrules.ValidateDirectExitRulesOptions(opts), "the fixture's loopback route comes before its ip_is_private reject")
	checked := opts
	route := *opts.Route
	route.Rules = slices.DeleteFunc(slices.Clone(route.Rules), func(r option.Rule) bool {
		return slices.Equal(r.DefaultOptions.IPCIDR, []string{"127.0.0.1/32"})
	})
	require.Len(t, route.Rules, len(opts.Route.Rules)-1, "the fixture's loopback route")
	checked.Route = &route
	require.NoError(t, peerrules.ValidateDirectExitRulesOptions(checked), "the fixture's rules but its loopback route")
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err, "exit box New")
	require.NoError(t, b.Start(), "exit box Start")
	t.Cleanup(func() { _ = b.Close() })
	return b.Router()
}

// countingRouter counts the streams and flows the exit hands the exit box, and those the exit
// box's rules rejected, which sing-box reports through onClose.
type countingRouter struct {
	adapter.ConnectionRouterEx
	routed, rejected atomic.Int64
}

func (r *countingRouter) onClose(onClose N.CloseHandlerFunc) N.CloseHandlerFunc {
	return func(err error) {
		if rule.IsRejected(err) {
			r.rejected.Add(1)
		}
		if onClose != nil {
			onClose(err)
		}
	}
}

func (r *countingRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.routed.Add(1)
	r.ConnectionRouterEx.RouteConnectionEx(ctx, conn, md, r.onClose(onClose))
}

func (r *countingRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.routed.Add(1)
	r.ConnectionRouterEx.RoutePacketConnectionEx(ctx, conn, md, r.onClose(onClose))
}

// udpListener listens for UDP on loopback, echoing what it reads if echo is set.
func udpListener(t *testing.T, echo bool) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	if echo {
		go func() {
			buf := make([]byte, 2048)
			for {
				n, from, err := pc.ReadFrom(buf)
				if err != nil {
					return
				}
				_, _ = pc.WriteTo(buf[:n], from)
			}
		}()
	}
	return pc
}

func countingHandler(hits *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	})
}

// requireRefused fails unless a GET of target through the rig's consumer fails, with the
// volunteer's exit box rejecting it by rule, and without reaching the egress or, if hits is
// not nil, the origin hits counts.
func requireRefused(t *testing.T, rig *directExitRig, target string, hits *atomic.Int64) {
	t.Helper()
	rejectedBefore := rig.router.rejected.Load()
	dialsBefore := rig.egressDials.Load()
	var hitsBefore int64
	if hits != nil {
		hitsBefore = hits.Load()
	}
	started := time.Now()
	require.False(t, proxiedGetOK(rig.proxy, target, 10*time.Second), "%s was not refused", target)
	// Only a guard against hangs: the rejection count below is what shows the refusal.
	require.Less(t, time.Since(started), 4*time.Second, "%s failed slowly", target)
	// sing-box reports the rejection through onClose, which can run just after the user's
	// side sees the stream fail.
	require.Eventually(t, func() bool { return rig.router.rejected.Load() > rejectedBefore },
		2*time.Second, 20*time.Millisecond, "the exit box's rules did not reject %s", target)
	require.Equal(t, dialsBefore, rig.egressDials.Load(), "%s went to the egress", target)
	if hits != nil {
		require.Equal(t, hitsBefore, hits.Load(), "%s reached its origin", target)
	}
}
