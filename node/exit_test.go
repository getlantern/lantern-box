//go:build with_quic

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// validExitConfig carries the abuse rules peerrules requires, and a log section RunExit must
// override.
const validExitConfig = `{
	"log":{"level":"debug"},
	"outbounds":[{"type":"direct","tag":"direct"}],
	"route":{
		"rule_set":[
			{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
		],
		"rules":[
			{"action":"reject","rule_set":["geosite-malware"]},
			{"action":"reject","rule_set":["geoip-malware"]},
			{"action":"reject","rule_set":["geosite-phishing"]},
			{"action":"reject","rule_set":["geosite-cryptominers"]},
			{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16"]},
			{"action":"reject","port":[25,465,587]}
		]
	}
}`

const exitConfigWithoutAbuseRules = `{"outbounds":[{"type":"direct","tag":"direct"}],"route":{"rules":[]}}`

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
			APIURL: srv.URL + "/v1", DeviceID: "dev", Version: "v9", Exit: e, NewBox: boxes.new,
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
	cancel()
	waitDone(t, done)
	assert.False(t, e.Options().Ready())
	closed, _ := b.isClosed()
	assert.True(t, closed)
}

func TestRunExitRefusesBadConfig(t *testing.T) {
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{APIURL: "http://127.0.0.1:1", DeviceID: "dev"}), "no exit")
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{DeviceID: "dev", Exit: enabledExit(t)}), "no API URL")
	assert.Error(t, RunExit(box.BaseContext(), ExitConfig{APIURL: "http://127.0.0.1:1", Exit: enabledExit(t)}), "no device ID")
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
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", DeviceID: "dev", Exit: e, NewBox: boxes.new}, ev)
	require.Eventually(t, func() bool { return registers.Load() >= 3 }, 5*time.Second, 5*time.Millisecond, "it keeps retrying")
	assert.Empty(t, boxes.all(), "no box is built from a config without the abuse rules")
	assert.False(t, e.Options().Ready())
	cancel()
	waitDone(t, done)
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
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", DeviceID: "dev", Exit: e, NewBox: boxes.new,
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
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", DeviceID: "dev", Exit: e, NewBox: boxes.new,
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
	done := runExitAsync(t, ctx, ExitConfig{APIURL: srv.URL + "/v1", DeviceID: deviceID, Exit: e,
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
