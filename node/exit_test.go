//go:build with_quic

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// validExitConfig carries the rules peerrules requires of a direct exit, and a log section and
// listeners RunExit must override.
const validExitConfig = `{
	"log":{"level":"debug"},
	"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":0}],
	"endpoints":[{"type":"amnezia","tag":"wg"}],
	"services":[{"type":"unbounded","tag":"unbounded-volunteer","discovery_srv":"https://freddie.test",
		"egress_addr":"wss://egress.test","stun_servers":["stun:192.0.2.1:3478"]}],
	"experimental":{"debug":{"listen":"127.0.0.1:6060"}},
	"ntp":{"enabled":true,"server":"time.example"},
	"outbounds":[{"type":"direct","tag":"direct"}],
	"route":{
		"rule_set":[
			{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
		],
		"rules":[
			{"action":"resolve","server":"dns"},
			{"action":"reject","ip_is_private":true},
			{"action":"reject","rule_set":["geosite-malware"]},
			{"action":"reject","rule_set":["geoip-malware"]},
			{"action":"reject","rule_set":["geosite-phishing"]},
			{"action":"reject","rule_set":["geosite-cryptominers"]},
			{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]},
			{"action":"reject","port":[25,465,587]},
			{"action":"route","outbound":"direct","udp_connect":true}
		]
	}
}`

const exitConfigWithoutAbuseRules = `{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"rules":[]}}`

// exitConfigWithoutAResolve has the abuse rules, but no resolve, so its IP rules would never
// see a domain's addresses.
var exitConfigWithoutAResolve = strings.Replace(validExitConfig, `{"action":"resolve","server":"dns"},`, ``, 1)

// exitConfigWithoutPrivateReject resolves, but doesn't refuse private addresses.
var exitConfigWithoutPrivateReject = strings.Replace(validExitConfig, `{"action":"reject","ip_is_private":true},`, ``, 1)

func exitCloud(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/peer/register", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func enabledExit(t *testing.T) *exit.Exit {
	t.Helper()
	e, err := exit.New(context.Background(), log.NewNOPFactory().Logger())
	require.NoError(t, err)
	e.SetEnabled(true)
	t.Cleanup(e.Close)
	return e
}

// exitEvents records, in order, the boxes built and closed and the routers handed to the exit.
type exitEvents struct {
	mu     sync.Mutex
	events []string
}

func (ev *exitEvents) add(format string, args ...any) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.events = append(ev.events, fmt.Sprintf(format, args...))
}

func (ev *exitEvents) list() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.events...)
}

type fakeExitRouter struct{ id int }

func (*fakeExitRouter) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return nil
}

func (*fakeExitRouter) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return nil
}

func (*fakeExitRouter) RouteConnectionEx(context.Context, net.Conn, adapter.InboundContext, N.CloseHandlerFunc) {
}

func (*fakeExitRouter) RoutePacketConnectionEx(context.Context, N.PacketConn, adapter.InboundContext, N.CloseHandlerFunc) {
}

type fakeExitBox struct {
	id                  int
	ev                  *exitEvents
	router              *fakeExitRouter
	builtAt             time.Time
	autoDetectInterface bool
	logDisabled         bool
	// extras counts the inbounds, endpoints, services, experimental and NTP sections it got.
	extras int

	mu       sync.Mutex
	closed   bool
	closedAt time.Time
}

func (b *fakeExitBox) Router() adapter.ConnectionRouterEx { return b.router }

func (b *fakeExitBox) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed, b.closedAt = true, time.Now()
		b.ev.add("close %d", b.id)
	}
	return nil
}

func (b *fakeExitBox) isClosed() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed, b.closedAt
}

// fakeExitBoxes is a NewBox recording what it builds.
type fakeExitBoxes struct {
	ev    *exitEvents
	mu    sync.Mutex
	boxes []*fakeExitBox
}

func (f *fakeExitBoxes) new(ctx context.Context, options string) (ExitBox, error) {
	opts, err := sjson.UnmarshalExtendedContext[option.Options](ctx, []byte(options))
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b := &fakeExitBox{id: len(f.boxes) + 1, ev: f.ev, router: &fakeExitRouter{id: len(f.boxes) + 1}, builtAt: time.Now()}
	b.autoDetectInterface = opts.Route != nil && opts.Route.AutoDetectInterface
	b.logDisabled = opts.Log != nil && opts.Log.Disabled
	b.extras = len(opts.Inbounds) + len(opts.Endpoints) + len(opts.Services)
	if opts.Experimental != nil {
		b.extras++
	}
	if opts.NTP != nil {
		b.extras++
	}
	f.boxes = append(f.boxes, b)
	f.ev.add("build %d", b.id)
	return b, nil
}

func (f *fakeExitBoxes) all() []*fakeExitBox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeExitBox(nil), f.boxes...)
}

func (f *fakeExitBoxes) last() *fakeExitBox {
	all := f.all()
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// recordingExit passes routers on to the real exit, noting which box's each is.
type recordingExit struct {
	*exit.Exit
	ev *exitEvents
}

func (r *recordingExit) SetRouter(router adapter.ConnectionRouterEx, validUntil time.Time) {
	if fr, ok := router.(*fakeExitRouter); ok {
		r.ev.add("router %d", fr.id)
	} else {
		r.ev.add("router nil")
	}
	r.Exit.SetRouter(router, validUntil)
}

// runExitAsync runs the registrar with fast retries, recording its routers in ev.
func runExitAsync(t *testing.T, ctx context.Context, cfg ExitConfig, ev *exitEvents) <-chan error {
	t.Helper()
	r, err := newExitRunner(cfg)
	require.NoError(t, err)
	r.minRetry, r.maxRetry = 10*time.Millisecond, 40*time.Millisecond
	r.exit = &recordingExit{Exit: cfg.Exit, ev: ev}
	done := make(chan error, 1)
	go func() { done <- r.loop(ctx) }()
	return done
}

func waitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunExit did not return")
	}
}

func TestRunExitRegistersWebRTCOnly(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	var hdr http.Header
	var registers atomic.Int32
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		registers.Add(1)
		mu.Lock()
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got)
		mu.Unlock()
		writeJSON(w, map[string]any{"server_config": validExitConfig, "rotation_interval_seconds": 3600})
	})
	e := enabledExit(t)
	boxes := &fakeExitBoxes{ev: &exitEvents{}}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunExit(ctx, ExitConfig{
			APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Version: "v9", Exit: e, NewBox: boxes.new,
			Header: func(h http.Header) { h.Set("X-Lantern-App", "lantern") },
		})
	}()
	require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, 10*time.Millisecond)
	mu.Lock()
	assert.Equal(t, map[string]any{"mode": "webrtc"}, got)
	assert.Equal(t, "lantern", hdr.Get("X-Lantern-App"), "the caller's headers win")
	assert.Equal(t, "dev", hdr.Get("X-Lantern-Device-Id"))
	assert.Equal(t, "v9", hdr.Get("X-Lantern-Version"))
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), registers.Load(), "the next registration waits lantern-cloud's rotation interval")
	b := boxes.last()
	assert.True(t, b.autoDetectInterface, "exit traffic leaves on the physical interface")
	assert.True(t, b.logDisabled, "the exit box never logs strangers' destinations")
	assert.Zero(t, b.extras, "the exit box opens no listeners and runs no services")
	cancel()
	waitDone(t, done)
	assert.False(t, e.Options().Ready())
	closed, _ := b.isClosed()
	assert.True(t, closed)
}

func TestRunExitRefusesBadConfig(t *testing.T) {
	c := http.DefaultClient
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{APIURL: "http://127.0.0.1:1", HTTPClient: c, DeviceID: "dev"}), "no exit")
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{HTTPClient: c, DeviceID: "dev", Exit: enabledExit(t)}), "no API URL")
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{APIURL: "http://127.0.0.1:1", HTTPClient: c, Exit: enabledExit(t)}), "no device ID")
	// Inside the VPN's process, a default client would resolve through the tunnel.
	assert.ErrorContains(t, RunExit(box.BaseContext(), ExitConfig{APIURL: "http://127.0.0.1:1", DeviceID: "dev", Exit: enabledExit(t)}),
		"HTTP client", "no HTTP client")
}

func TestRunExitRefusesConfigWithoutAbuseRules(t *testing.T) {
	var registers atomic.Int32
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		registers.Add(1)
		writeJSON(w, map[string]any{"server_config": exitConfigWithoutAbuseRules})
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	boxes := &fakeExitBoxes{ev: ev}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e, NewBox: boxes.new}, ev)
	require.Eventually(t, func() bool { return registers.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "it keeps retrying")
	assert.Empty(t, boxes.all(), "no box is built from a config without the abuse rules")
	assert.False(t, e.Options().Ready())
	cancel()
	waitDone(t, done)
}

// sing-box's IP rules see a domain's addresses only after a resolve, so a direct exit's config
// must resolve, then refuse private addresses.
func TestRunExitRefusesConfigWithoutAResolveAndPrivateReject(t *testing.T) {
	for name, config := range map[string]string{
		"no resolve":        exitConfigWithoutAResolve,
		"no private reject": exitConfigWithoutPrivateReject,
	} {
		t.Run(name, func(t *testing.T) {
			var registers atomic.Int32
			srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
				registers.Add(1)
				writeJSON(w, map[string]any{"server_config": config})
			})
			e := enabledExit(t)
			ev := &exitEvents{}
			boxes := &fakeExitBoxes{ev: ev}
			ctx, cancel := context.WithCancel(box.BaseContext())
			defer cancel()
			done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e, NewBox: boxes.new}, ev)
			require.Eventually(t, func() bool { return registers.Load() >= 2 }, 5*time.Second, 5*time.Millisecond)
			assert.Empty(t, boxes.all(), "no box is built from a config that skips IP rules for domains")
			assert.False(t, e.Options().Ready())
			cancel()
			waitDone(t, done)
		})
	}
}

func TestRunExitRotatesWithoutAGap(t *testing.T) {
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"server_config": validExitConfig, "rotation_interval_seconds": 3600})
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	boxes := &fakeExitBoxes{ev: ev}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e, NewBox: boxes.new,
		RotationInterval: 100 * time.Millisecond}, ev)
	require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, time.Millisecond)

	var gap atomic.Bool
	stop := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !e.Options().Ready() {
				gap.Store(true)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	require.Eventually(t, func() bool { return len(boxes.all()) >= 3 }, 5*time.Second, 5*time.Millisecond)
	close(stop)
	<-polled
	assert.False(t, gap.Load(), "the exit stays ready across rotations")
	cancel()
	waitDone(t, done)

	events := ev.list()
	require.GreaterOrEqual(t, len(events), 10)
	assert.Equal(t, []string{"build 1", "router 1", "build 2", "router 2", "close 1", "build 3", "router 3", "close 2"}, events[:8],
		"each box is built and its router swapped in before the previous box closes")
	assert.Equal(t, []string{"router nil", fmt.Sprintf("close %d", len(boxes.all()))}, events[len(events)-2:],
		"on shutdown the router is cleared before the box closes")
}

func TestRunExitKeepsServingUntilExpiryWhenRefused(t *testing.T) {
	var registers atomic.Int32
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		if registers.Add(1) > 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"server_config": validExitConfig})
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	boxes := &fakeExitBoxes{ev: ev}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	const rotation = 100 * time.Millisecond
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e, NewBox: boxes.new,
		RotationInterval: rotation}, ev)
	require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return registers.Load() >= 2 }, 5*time.Second, time.Millisecond)
	b := boxes.last()
	if time.Since(b.builtAt) < 2*rotation-20*time.Millisecond {
		assert.True(t, e.Options().Ready(), "a refusal leaves the current box serving until it expires")
	}
	require.Eventually(t, func() bool { closed, _ := b.isClosed(); return closed }, 5*time.Second, 5*time.Millisecond)
	_, closedAt := b.isClosed()
	assert.GreaterOrEqual(t, closedAt.Sub(b.builtAt), 2*rotation, "the box serves until two rotations have passed")
	assert.False(t, e.Options().Ready())
	assert.Len(t, boxes.all(), 1)
	assert.Equal(t, []string{"build 1", "router 1", "router nil", "close 1"}, ev.list())
	cancel()
	waitDone(t, done)
	assert.Equal(t, []string{"build 1", "router 1", "router nil", "close 1"}, ev.list(), "nothing is left to close")
}

// holdOffs records what RunExit reports through OnHoldOff.
type holdOffs struct {
	mu    sync.Mutex
	until []time.Time
}

func (h *holdOffs) record(until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.until = append(h.until, until)
}

func (h *holdOffs) list() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.until...)
}

// lantern-cloud's Retry-After on a 429 or 503 is the least RunExit waits, and RunExit reports
// the hold-off so the embedder can keep it across a restart. A success clears it.
func TestRunExitHonorsRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			var at []time.Time
			srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				at = append(at, time.Now())
				n := len(at)
				mu.Unlock()
				if n == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				writeJSON(w, map[string]any{"server_config": validExitConfig, "rotation_interval_seconds": 3600})
			})
			e := enabledExit(t)
			ev := &exitEvents{}
			held := &holdOffs{}
			ctx, cancel := context.WithCancel(box.BaseContext())
			defer cancel()
			started := time.Now()
			done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e,
				NewBox: (&fakeExitBoxes{ev: ev}).new, OnHoldOff: held.record}, ev)
			require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, 5*time.Millisecond)
			mu.Lock()
			require.Len(t, at, 2)
			gap := at[1].Sub(at[0])
			mu.Unlock()
			assert.GreaterOrEqual(t, gap, 950*time.Millisecond, "the retry waits out Retry-After, not the 10ms back-off")
			got := held.list()
			require.Len(t, got, 2)
			assert.WithinDuration(t, started.Add(time.Second), got[0], 500*time.Millisecond)
			assert.True(t, got[1].IsZero(), "a registration clears the hold-off")
			cancel()
			waitDone(t, done)
		})
	}
}

// A busy answer without Retry-After still holds off, for the back-off, and reports it. Other
// failures hold off nothing beyond this run.
func TestRunExitReportsTheBusyBackOff(t *testing.T) {
	var registers atomic.Int32
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		if registers.Add(1)%2 == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	held := &holdOffs{}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e,
		NewBox: (&fakeExitBoxes{ev: ev}).new, OnHoldOff: held.record}, ev)
	require.Eventually(t, func() bool { return registers.Load() >= 4 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	waitDone(t, done)
	got := held.list()
	require.NotEmpty(t, got)
	assert.LessOrEqual(t, len(got), int(registers.Load()+1)/2, "only the 503s hold off")
	for _, until := range got {
		assert.False(t, until.IsZero())
	}
}

// NotBefore carries a hold-off across a restart: RunExit calls lantern-cloud no sooner.
func TestRunExitWaitsForNotBefore(t *testing.T) {
	var mu sync.Mutex
	var first time.Time
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if first.IsZero() {
			first = time.Now()
		}
		mu.Unlock()
		writeJSON(w, map[string]any{"server_config": validExitConfig, "rotation_interval_seconds": 3600})
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	notBefore := time.Now().Add(300 * time.Millisecond)
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: "dev", Exit: e,
		NewBox: (&fakeExitBoxes{ev: ev}).new, NotBefore: notBefore}, ev)
	require.Eventually(t, func() bool { return e.Options().Ready() }, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	assert.False(t, first.Before(notBefore), "registered before NotBefore")
	mu.Unlock()
	cancel()
	waitDone(t, done)

	// Canceled while waiting, it returns without calling lantern-cloud.
	var calls atomic.Int32
	srv2 := exitCloud(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	ctx2, cancel2 := context.WithCancel(box.BaseContext())
	done = runExitAsync(t, ctx2, ExitConfig{APIURL: srv2.URL + "/v1", HTTPClient: srv2.Client(), DeviceID: "dev", Exit: e,
		NewBox: (&fakeExitBoxes{ev: ev}).new, NotBefore: time.Now().Add(time.Hour)}, ev)
	time.Sleep(50 * time.Millisecond)
	cancel2()
	waitDone(t, done)
	assert.Zero(t, calls.Load())
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"soon":                          0,
		"Wed, 18 Nov 2026 12:00:30 GMT": 30 * time.Second,
		"Wed, 18 Nov 2026 11:00:00 GMT": 0,
		"999999999999":                  maxHoldOff,
	} {
		assert.Equal(t, want, parseRetryAfter(in, now), in)
	}
}

type exitLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *exitLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *exitLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunExitLogsNoSecrets(t *testing.T) {
	const deviceID = "device-secret-4b1d"
	const body = "device device-secret-4b1d is banned from 203.0.113.9"
	buf := &exitLogBuffer{}
	factory := log.NewDefaultFactory(context.Background(), log.Formatter{}, buf, "", nil, false)
	factory.SetLevel(log.LevelTrace)
	require.NoError(t, factory.Start())
	prev := log.StdLogger()
	log.SetStdLogger(factory.Logger())
	t.Cleanup(func() {
		log.SetStdLogger(prev)
		_ = factory.Close()
	})

	var registers atomic.Int32
	srv := exitCloud(t, func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusForbidden
		if registers.Add(1)%2 == 0 {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	e := enabledExit(t)
	ev := &exitEvents{}
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", HTTPClient: srv.Client(), DeviceID: deviceID, Exit: e,
		NewBox: (&fakeExitBoxes{ev: ev}).new}, ev)
	require.Eventually(t, func() bool { return registers.Load() >= 4 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	waitDone(t, done)

	out := buf.String()
	assert.Contains(t, out, "lantern-cloud refused this device as a direct exit")
	assert.Contains(t, out, "500")
	assert.NotContains(t, out, deviceID)
	assert.NotContains(t, out, "banned")
	assert.NotContains(t, out, "203.0.113.9")
}

func TestNewExitSingBoxRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	b, err := newExitSingBox(ctx, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	require.NoError(t, err)
	assert.NotNil(t, b.Router())
	require.NoError(t, b.Close())
}

func TestNewExitSingBoxKeepsItsServicesToItself(t *testing.T) {
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	sentinel := log.NewDefaultFactory(context.Background(), log.Formatter{}, io.Discard, "", nil, false)
	service.MustRegister[log.Factory](ctx, sentinel)
	b, err := newExitSingBox(ctx, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()
	assert.Same(t, sentinel, service.FromContext[log.Factory](ctx), "the caller's log factory is still its own")
	assert.Nil(t, service.FromContext[adapter.OutboundManager](ctx), "the exit box's managers stay out of the caller's registry")
}

// sentinelFactory is a caller's log factory that records whether anything used or closed it.
type sentinelFactory struct {
	log.Factory
	used, closed atomic.Bool
}

func (f *sentinelFactory) Logger() log.ContextLogger { f.used.Store(true); return f.Factory.Logger() }
func (f *sentinelFactory) NewLogger(tag string) log.ContextLogger {
	f.used.Store(true)
	return f.Factory.NewLogger(tag)
}
func (f *sentinelFactory) Close() error { f.closed.Store(true); return nil }

// The exit box routes strangers' traffic, and sing-box would log it through a log factory it
// finds in its context, ignoring log.disabled, and close that factory with the box. So it must
// never take its caller's.
func TestNewExitSingBoxNeverUsesTheCallersLogFactory(t *testing.T) {
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	sentinel := &sentinelFactory{Factory: log.NewNOPFactory()}
	service.MustRegister[log.Factory](ctx, sentinel)
	b, err := newExitSingBox(ctx, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	require.NoError(t, err)
	require.NoError(t, b.Close())
	assert.False(t, sentinel.used.Load(), "the exit box logs nothing through the caller's factory")
	assert.False(t, sentinel.closed.Load(), "closing the exit box leaves the caller's factory open")
}
