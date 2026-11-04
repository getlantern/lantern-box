package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	C "github.com/getlantern/lantern-box/constant"
	LO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/tracker/peerconn"
)

const volunteerConfig = `{"inbounds":[{"type":"direct","tag":"in","listen":"127.0.0.1","listen_port":30001}],` +
	`"services":[{"type":"unbounded","tag":"unbounded-volunteer","discovery_srv":"https://freddie.test","egress_addr":"wss://egress.test","stun_servers":["stun:192.0.2.1:3478"]}]}`

// fakeCloud is lantern-cloud's peer API, recording what the node sends.
type fakeCloud struct {
	t *testing.T

	mu           sync.Mutex
	calls        []string
	registers    []registerRequest
	headers      []http.Header
	heartbeats   []heartbeatRequest
	deregistered []string
	routes       int
	// newRouteIDs gives every registration a new route ID. Otherwise, like
	// lantern-cloud, a node registering the same port gets the same one.
	newRouteIDs bool
	// verifyStatus and heartbeatStatus, when set, are answered instead of 200.
	verifyStatus    int
	heartbeatStatus int
	// registerFails, when set, decides from the registration's number,
	// counting from 1, whether it fails: with a status, or by dropping the
	// connection when the status is -1.
	registerFails func(n int) int
	config        string
	// noConfig answers registrations with a route but no config.
	noConfig bool
}

func (f *fakeCloud) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	f.headers = append(f.headers, r.Header.Clone())
	switch r.URL.Path {
	case "/v1/peer/register":
		var req registerRequest
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
		f.registers = append(f.registers, req)
		if f.registerFails != nil {
			switch status := f.registerFails(len(f.registers)); status {
			case 0:
			case -1:
				conn, _, err := w.(http.Hijacker).Hijack()
				require.NoError(f.t, err)
				_ = conn.Close()
				return
			default:
				w.WriteHeader(status)
				return
			}
		}
		if f.routes == 0 || f.newRouteIDs {
			f.routes++
		}
		config := f.config
		if f.noConfig {
			config = ""
		}
		_ = json.NewEncoder(w).Encode(registerResponse{RouteID: fmt.Sprintf("route-%d", f.routes), ServerConfig: config, HeartbeatIntervalSeconds: 150})
	case "/v1/peer/verify":
		if f.verifyStatus != 0 {
			w.WriteHeader(f.verifyStatus)
		}
	case "/v1/peer/heartbeat":
		var req heartbeatRequest
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
		f.heartbeats = append(f.heartbeats, req)
		if f.heartbeatStatus != 0 {
			w.WriteHeader(f.heartbeatStatus)
		}
	case "/v1/peer/deregister":
		var req lifecycleRequest
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))
		f.deregistered = append(f.deregistered, req.RouteID)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCloud) snapshot() (calls []string, registers []registerRequest, heartbeats []heartbeatRequest, deregistered []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([]registerRequest(nil), f.registers...),
		append([]heartbeatRequest(nil), f.heartbeats...), append([]string(nil), f.deregistered...)
}

// fakeBoxes records the boxes the node starts, and fails if two are open at
// once, since real ones would share the node's port.
type fakeBoxes struct {
	mu      sync.Mutex
	options []string
	open    int
	overlap atomic.Bool
}

type fakeBox struct {
	b      *fakeBoxes
	closed atomic.Bool
}

func (b *fakeBox) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		b.b.mu.Lock()
		b.b.open--
		b.b.mu.Unlock()
	}
	return nil
}

func (f *fakeBoxes) factory(_ context.Context, options string) (boxService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.options = append(f.options, options)
	f.open++
	if f.open > 1 {
		f.overlap.Store(true)
	}
	return &fakeBox{b: f}, nil
}

func (f *fakeBoxes) state() (started, open int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.options), f.open
}

func newTestNode(t *testing.T, cloud *fakeCloud, boxes *fakeBoxes, cfg Config) *node {
	srv := httptest.NewServer(http.HandlerFunc(cloud.handler))
	t.Cleanup(srv.Close)
	if cloud.config == "" {
		cloud.config = volunteerConfig
	}
	n := &node{
		cfg: cfg,
		api: &api{httpClient: srv.Client(), baseURL: srv.URL + "/v1", deviceID: "device-1",
			userID: cfg.UserID, featureOverrides: cfg.FeatureOverrides, version: "v9"},
		port:              24680,
		newBox:            boxes.factory,
		clients:           newClientCounter(),
		rotation:          time.Hour,
		heartbeatOverride: 20 * time.Millisecond,
	}
	if cfg.RotationInterval > 0 {
		n.rotation = cfg.RotationInterval
	}
	return n
}

func runNode(t *testing.T, n *node) (stop func()) {
	ctx, cancel := context.WithCancel(box.BaseContext())
	done := make(chan error, 1)
	go func() { done <- n.run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("node did not stop")
		}
	}
}

// A node registers as a pool-serving volunteer on its port, applies the
// operator's limits to the volunteer service, has lantern-cloud verify it,
// heartbeats with its client count, and on shutdown closes its box and
// deregisters.
func TestNodeServes(t *testing.T) {
	cloud := &fakeCloud{t: t}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{MaxPeers: 7, BandwidthLimit: "10mbit", UserID: "user-9", FeatureOverrides: "unbounded"})
	n.clients.listen(peerconn.Event{State: 1, Source: "198.51.100.1:5000"})
	n.clients.listen(peerconn.Event{State: 1, Source: "198.51.100.1:5001"})
	n.clients.listen(peerconn.Event{State: 1, Source: "198.51.100.2:5000"})

	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		_, _, hb, _ := cloud.snapshot()
		return len(hb) >= 2
	}, 5*time.Second, 10*time.Millisecond)
	stop()

	calls, registers, heartbeats, deregistered := cloud.snapshot()
	assert.Equal(t, []string{"/v1/peer/register", "/v1/peer/verify", "/v1/peer/heartbeat"}, calls[:3])
	assert.Equal(t, "/v1/peer/deregister", calls[len(calls)-1])
	require.Len(t, registers, 1)
	assert.Equal(t, registerRequest{ExternalPort: 24680, InternalPort: 24680, ServePool: true, RunsUnboundedVolunteer: true}, registers[0])
	assert.Equal(t, heartbeatRequest{RouteID: "route-1", ActiveClients: 2}, heartbeats[0], "two distinct client IPs")
	assert.Equal(t, []string{"route-1"}, deregistered)

	h := cloud.headers[0]
	assert.Equal(t, "device-1", h.Get("X-Lantern-Device-Id"))
	assert.Equal(t, "user-9", h.Get("X-Lantern-User-Id"))
	assert.Equal(t, "unbounded", h.Get("X-Lantern-Feature-Override"))
	assert.Equal(t, "lantern-box", h.Get("X-Lantern-App"))

	started, open := boxes.state()
	assert.Equal(t, 1, started)
	assert.Zero(t, open, "the box is closed on shutdown")
	vol := volunteerOf(t, boxes.options[0])
	assert.Equal(t, 7, vol.MaxPeers)
	assert.Equal(t, "10mbit", vol.BandwidthLimit)
	assert.Equal(t, "https://freddie.test", vol.DiscoverySrv, "lantern-cloud's settings are kept")
}

// When lantern-cloud no longer knows the route, the node closes its box and
// registers again.
func TestNodeRegistersAgainWhenRouteIsLost(t *testing.T) {
	cloud := &fakeCloud{t: t, heartbeatStatus: http.StatusNotFound}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{})
	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		_, registers, _, _ := cloud.snapshot()
		return len(registers) >= 2
	}, 5*time.Second, 10*time.Millisecond)
	stop()
	assert.False(t, boxes.overlap.Load(), "the old box closes before the new one starts")
}

// Rotation registers the node's route again, which lantern-cloud keys by
// address and port so its ID stays the same, and restarts the box: the old
// one closes before the new one takes the port, and the route, still live, is
// not deregistered.
func TestNodeRotates(t *testing.T) {
	cloud := &fakeCloud{t: t}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{RotationInterval: 50 * time.Millisecond})
	n.heartbeatOverride = time.Hour
	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		started, _ := boxes.state()
		return started >= 3
	}, 5*time.Second, 10*time.Millisecond)
	_, registers, _, deregistered := cloud.snapshot()
	assert.GreaterOrEqual(t, len(registers), 3)
	assert.Empty(t, deregistered, "rotation keeps the route it registered again")
	stop()

	_, _, _, deregistered = cloud.snapshot()
	assert.Equal(t, []string{"route-1"}, deregistered, "only shutdown deregisters it")
	assert.False(t, boxes.overlap.Load(), "the old box closes before the new one starts")
	_, open := boxes.state()
	assert.Zero(t, open)
}

// If lantern-cloud hands out a new route ID on rotation, the old route is no
// longer this node's to serve, and is deregistered.
func TestNodeRotatesToANewRoute(t *testing.T) {
	cloud := &fakeCloud{t: t, newRouteIDs: true}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{RotationInterval: 50 * time.Millisecond})
	n.heartbeatOverride = time.Hour
	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		_, _, _, deregistered := cloud.snapshot()
		return len(deregistered) >= 1
	}, 5*time.Second, 10*time.Millisecond)
	stop()
	_, _, _, deregistered := cloud.snapshot()
	assert.Equal(t, "route-1", deregistered[0])
	assert.False(t, boxes.overlap.Load())
}

// When lantern-cloud refuses a rotation's registration with a 4xx, nothing
// changed on its side, so the node keeps serving the route it has.
func TestRotationKeepsServingWhenRefused(t *testing.T) {
	cloud := &fakeCloud{t: t, registerFails: func(n int) int {
		if n > 1 {
			return http.StatusTooManyRequests
		}
		return 0
	}}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{RotationInterval: 30 * time.Millisecond})
	n.heartbeatOverride = time.Hour
	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		_, registers, _, _ := cloud.snapshot()
		return len(registers) >= 3
	}, 5*time.Second, 10*time.Millisecond)
	started, open := boxes.state()
	_, _, _, deregistered := cloud.snapshot()
	stop()
	assert.Equal(t, 1, started, "the box keeps running")
	assert.Equal(t, 1, open)
	assert.Empty(t, deregistered)
}

// When a rotation's registration fails without a definite answer, a dropped
// connection or a 5xx, lantern-cloud may have switched the route to
// credentials the box doesn't have, so the node closes it, deregisters, and
// starts over.
func TestRotationStartsOverWhenUncertain(t *testing.T) {
	for name, failure := range map[string]int{"dropped": -1, "bad gateway": http.StatusBadGateway} {
		t.Run(name, func(t *testing.T) {
			cloud := &fakeCloud{t: t, registerFails: func(n int) int {
				if n == 2 {
					return failure
				}
				return 0
			}}
			boxes := &fakeBoxes{}
			n := newTestNode(t, cloud, boxes, Config{RotationInterval: time.Hour})
			ctx, cancel := context.WithCancel(box.BaseContext())
			defer cancel()
			s, err := n.start(ctx)
			require.NoError(t, err)
			next, err := n.rotate(ctx, s)
			require.Error(t, err)
			assert.Nil(t, next)
			_, _, _, deregistered := cloud.snapshot()
			assert.Equal(t, []string{"route-1"}, deregistered)
			_, open := boxes.state()
			assert.Zero(t, open)
		})
	}
}

// Refused rotations closer together than heartbeats don't hold the heartbeats
// off, which would let the route expire.
func TestRefusedRotationsKeepHeartbeating(t *testing.T) {
	cloud := &fakeCloud{t: t, registerFails: func(n int) int {
		if n > 1 {
			return http.StatusTooManyRequests
		}
		return 0
	}}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{RotationInterval: 20 * time.Millisecond})
	n.heartbeatOverride = 100 * time.Millisecond
	stop := runNode(t, n)
	require.Eventually(t, func() bool {
		_, _, heartbeats, _ := cloud.snapshot()
		return len(heartbeats) >= 2
	}, 5*time.Second, 10*time.Millisecond)
	stop()
}

// A registration answered with a route but no config is deregistered.
func TestStartDeregistersARouteWithoutConfig(t *testing.T) {
	cloud := &fakeCloud{t: t, noConfig: true}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{})
	_, err := n.start(box.BaseContext())
	require.ErrorContains(t, err, "no config")
	_, _, _, deregistered := cloud.snapshot()
	assert.Equal(t, []string{"route-1"}, deregistered)
	started, _ := boxes.state()
	assert.Zero(t, started)
}

// The API client follows a redirect within lantern-cloud's origin, and
// refuses one to another host, which would receive the device ID.
func TestAPIRedirectsStayInOrigin(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(other.Close)
	var target string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/v1/moved/peer/heartbeat", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := &api{httpClient: sameOrigin(srv.Client()), baseURL: srv.URL + "/v1", deviceID: "device-1"}

	target = "/v1/moved/peer/heartbeat"
	require.NoError(t, a.heartbeat(context.Background(), "route-1", 0))

	target = other.URL + "/peer/heartbeat"
	require.ErrorContains(t, a.heartbeat(context.Background(), "route-1", 0), "another origin")
	assert.Zero(t, elsewhere.Load(), "the other host never sees the request")
}

// Limits the volunteer service would refuse are refused before the node
// registers or builds a box.
func TestRunRefusesBadLimits(t *testing.T) {
	err := Run(box.BaseContext(), Config{StateDir: t.TempDir(), BandwidthLimit: "lots"})
	assert.ErrorContains(t, err, "bandwidth limit")
	err = Run(box.BaseContext(), Config{StateDir: t.TempDir(), MaxPeers: -1})
	assert.ErrorContains(t, err, "max peers")
}

// A second node on the same state directory stops before registering.
func TestRunRefusesASharedStateDir(t *testing.T) {
	dir := t.TempDir()
	lock := flock.New(filepath.Join(dir, "node.lock"))
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	defer func() { _ = lock.Unlock() }()
	err = Run(box.BaseContext(), Config{StateDir: dir, APIURL: "http://127.0.0.1:1"})
	assert.ErrorContains(t, err, "another lantern-box serve")
}

// A route lantern-cloud can't verify is deregistered and its box closed, and
// the node keeps trying.
func TestNodeRetriesAfterFailedVerify(t *testing.T) {
	cloud := &fakeCloud{t: t, verifyStatus: http.StatusUnprocessableEntity}
	boxes := &fakeBoxes{}
	n := newTestNode(t, cloud, boxes, Config{})
	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	_, err := n.start(ctx)
	require.Error(t, err)
	_, _, _, deregistered := cloud.snapshot()
	assert.Equal(t, []string{"route-1"}, deregistered)
	_, open := boxes.state()
	assert.Zero(t, open)
}

// A config without the volunteer service, as lantern-cloud sends a node in a
// censored country, is served as it is.
func TestApplyOperatorLimitsWithoutVolunteer(t *testing.T) {
	in := `{"inbounds":[{"type":"direct","tag":"in","listen":"127.0.0.1","listen_port":30001}]}`
	out, found, err := applyOperatorLimits(box.BaseContext(), in, 7, "10mbit")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, in, out)
}

// The first run picks a device ID and a random port and saves them; later
// runs reuse them.
func TestLoadStatePersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := loadState(dir)
	require.NoError(t, err)
	assert.Len(t, first.DeviceID, 32)
	assert.GreaterOrEqual(t, int(first.Port), minPort)
	assert.Less(t, int(first.Port), maxPort)

	second, err := loadState(dir)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	info, err := os.Stat(filepath.Join(dir, stateFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestClientCounter(t *testing.T) {
	c := newClientCounter()
	c.listen(peerconn.Event{State: 1, Source: "198.51.100.1:1"})
	c.listen(peerconn.Event{State: 1, Source: "198.51.100.1:2"})
	c.listen(peerconn.Event{State: 1, Source: "[2001:db8::1]:3"})
	c.listen(peerconn.Event{State: 1, Source: ""})
	assert.Equal(t, 2, c.count())
	c.listen(peerconn.Event{State: -1, Source: "198.51.100.1:1"})
	assert.Equal(t, 2, c.count(), "one connection from the first IP is still open")
	c.listen(peerconn.Event{State: -1, Source: "198.51.100.1:2"})
	assert.Equal(t, 1, c.count())
}

func volunteerOf(t *testing.T, options string) *LO.UnboundedVolunteerOptions {
	t.Helper()
	opts, err := sjson.UnmarshalExtendedContext[option.Options](box.BaseContext(), []byte(options))
	require.NoError(t, err)
	for _, svc := range opts.Services {
		if svc.Type == C.TypeUnbounded {
			vol, ok := svc.Options.(*LO.UnboundedVolunteerOptions)
			require.True(t, ok)
			return vol
		}
	}
	t.Fatal("no volunteer service")
	return nil
}

// The node's box factory starts a real box from lantern-cloud's JSON, the
// volunteer service included, through the registries in the node's context,
// and closes it. Discovery and egress are a local server that never answers
// usefully, so nothing leaves the machine.
func TestNewSingBoxRunsVolunteer(t *testing.T) {
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unreachable.Close)
	options := fmt.Sprintf(`{"inbounds":[{"type":"direct","tag":"in","listen":"127.0.0.1","listen_port":0}],`+
		`"services":[{"type":"unbounded","tag":"unbounded-volunteer","discovery_srv":%q,"egress_addr":%q,"stun_servers":["stun:127.0.0.1:9"]}]}`,
		unreachable.URL, "ws"+unreachable.URL[len("http"):])
	limited, found, err := applyOperatorLimits(box.BaseContext(), options, 3, "1mbit")
	require.NoError(t, err)
	require.True(t, found)

	ctx, cancel := context.WithCancel(box.BaseContext())
	defer cancel()
	b, err := newSingBox(ctx, limited)
	require.NoError(t, err)
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case err := <-closed:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("box did not close")
	}
}
