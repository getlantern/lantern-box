package unbounded

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/getlantern/broflake/common"
	"github.com/pion/stun/v4"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/include"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

func validVolunteerOptions() option.UnboundedVolunteerOptions {
	return option.UnboundedVolunteerOptions{
		DiscoverySrv: "https://freddie.example",
		EgressAddr:   "wss://egress.example",
		STUNServers:  []string{"stun:192.0.2.1:3478"},
	}
}

func newTestVolunteer(t *testing.T, opts option.UnboundedVolunteerOptions) *Volunteer {
	t.Helper()
	s, err := NewVolunteer(context.Background(), sblog.NewNOPFactory().Logger(), "volunteer", opts)
	require.NoError(t, err)
	return s.(*Volunteer)
}

func TestNewVolunteer_RequiresDiscoveryEgressAndSTUN(t *testing.T) {
	for name, mutate := range map[string]func(*option.UnboundedVolunteerOptions){
		"discovery_srv": func(o *option.UnboundedVolunteerOptions) { o.DiscoverySrv = "" },
		"egress_addr":   func(o *option.UnboundedVolunteerOptions) { o.EgressAddr = "" },
		"stun_servers":  func(o *option.UnboundedVolunteerOptions) { o.STUNServers = nil },
		"max_peers":     func(o *option.UnboundedVolunteerOptions) { o.MaxPeers = -1 },
		"stun_batch":    func(o *option.UnboundedVolunteerOptions) { o.STUNBatchSize = -1 },
		"bus_buffer":    func(o *option.UnboundedVolunteerOptions) { o.BusBufferSz = -1 },
		"nat_fail":      func(o *option.UnboundedVolunteerOptions) { o.NATFailTimeout = -1 },
		"patience":      func(o *option.UnboundedVolunteerOptions) { o.Patience = -1 },
		"error_backoff": func(o *option.UnboundedVolunteerOptions) { o.ErrorBackoff = -1 },
		"egress_conn":   func(o *option.UnboundedVolunteerOptions) { o.EgressConnectTimeout = -1 },
		"egress_back":   func(o *option.UnboundedVolunteerOptions) { o.EgressErrorBackoff = -1 },
		"bandwidth":     func(o *option.UnboundedVolunteerOptions) { o.BandwidthLimit = "fast" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := validVolunteerOptions()
			mutate(&opts)
			_, err := NewVolunteer(context.Background(), sblog.NewNOPFactory().Logger(), "volunteer", opts)
			require.Error(t, err)
		})
	}
}

func TestNewVolunteer_MapsOptions(t *testing.T) {
	opts := validVolunteerOptions()
	opts.DiscoveryEndpoint = "/v1/signal"
	opts.EgressEndpoint = "/ws"
	opts.MaxPeers = 25
	opts.STUNBatchSize = 3
	opts.TargetCountries = []string{"CN", "IR"}
	opts.Tuning = map[string]string{"ice_max_binding_requests": "40", "bogus": "1"}
	v := newTestVolunteer(t, opts)

	assert.Equal(t, "widget", v.bfOpt.ClientType, "a volunteer is broflake's widget role")
	assert.Equal(t, 25, v.bfOpt.CTableSize, "max_peers sets the volunteer's concurrent consumers")
	assert.Equal(t, 25, v.bfOpt.PTableSize, "each WebRTC slot needs the egress connection with its index")
	assert.Equal(t, "https://freddie.example", v.rtcOpt.DiscoverySrv)
	assert.Equal(t, "/v1/signal", v.rtcOpt.Endpoint)
	assert.Equal(t, "wss://egress.example", v.egOpt.Addr)
	assert.Equal(t, "/ws", v.egOpt.Endpoint)
	assert.EqualValues(t, 3, v.rtcOpt.STUNBatchSize)
	assert.Equal(t, []string{"CN", "IR"}, v.rtcOpt.TargetCountries)
	assert.Equal(t, common.VolunteerKindServer, v.rtcOpt.VolunteerKind, "freddie must count a node as a server volunteer")
	assert.False(t, v.rtcOpt.VolunteerMapped, "no port is mapped yet")
	assert.EqualValues(t, 40, v.rtcOpt.Tuning.ICEMaxBindingRequests)
	assert.Equal(t, defaultStatsInterval, v.statsInterval)
	assert.NotNil(t, v.rtcOpt.OnSTUNPreflight, "STUN check failures must reach the operator's log")
}

// A bandwidth limit gives broflake a rate-limited Net for the volunteer's WebRTC and STUN
// sockets, at the limit in each direction. Without one, broflake uses the host's stack.
func TestNewVolunteer_BandwidthLimit(t *testing.T) {
	opts := validVolunteerOptions()
	opts.BandwidthLimit = "20mbit"
	v := newTestVolunteer(t, opts)
	ln, ok := v.rtcOpt.Net.(*limitedNet)
	require.True(t, ok, "Net is %T, want *limitedNet", v.rtcOpt.Net)
	assert.Equal(t, 2_500_000.0, float64(ln.tx.Limit()))
	assert.Equal(t, 2_500_000.0, float64(ln.rx.Limit()))
	assert.Equal(t, byteRate(2_500_000), v.bandwidth)

	assert.Nil(t, newTestVolunteer(t, validVolunteerOptions()).rtcOpt.Net)
	opts.BandwidthLimit = "0"
	assert.Nil(t, newTestVolunteer(t, opts).rtcOpt.Net, `"0" means no limit`)
}

func TestNewVolunteer_DefaultTablesMatch(t *testing.T) {
	v := newTestVolunteer(t, validVolunteerOptions())
	assert.Equal(t, v.bfOpt.CTableSize, v.bfOpt.PTableSize)
}

func TestNewVolunteer_NATFailTimeout(t *testing.T) {
	opts := validVolunteerOptions()
	opts.NATFailTimeout = 7
	assert.Equal(t, 7*time.Second, newTestVolunteer(t, opts).rtcOpt.NATFailTimeout)

	// lantern-cloud's unbounded_volunteer_tuning key wins over the plain option.
	opts.Tuning = map[string]string{natFailTimeoutKey: "12000"}
	assert.Equal(t, 12*time.Second, newTestVolunteer(t, opts).rtcOpt.NATFailTimeout)

	// Out of range, it is ignored and the option stands.
	opts.Tuning = map[string]string{natFailTimeoutKey: "500"}
	assert.Equal(t, 7*time.Second, newTestVolunteer(t, opts).rtcOpt.NATFailTimeout)
}

func TestVolunteerTuningLeavesTheConfigMapAlone(t *testing.T) {
	in := map[string]string{natFailTimeoutKey: "8000", "ice_max_binding_requests": "40"}
	natFail, tuning, ignored := volunteerTuning(in)
	assert.Equal(t, 8*time.Second, natFail)
	assert.EqualValues(t, 40, tuning.ICEMaxBindingRequests)
	assert.Empty(t, ignored)
	assert.Len(t, in, 2, "the caller's map must not be modified")
}

func TestNewVolunteer_StatsInterval(t *testing.T) {
	opts := validVolunteerOptions()
	opts.StatsInterval = 30
	assert.Equal(t, 30*time.Second, newTestVolunteer(t, opts).statsInterval)
	opts.StatsInterval = -1
	assert.Zero(t, newTestVolunteer(t, opts).statsInterval, "a negative interval turns the stats line off")
}

func TestVolunteerStatsFollowBroflakeCallbacks(t *testing.T) {
	v := newTestVolunteer(t, validVolunteerOptions())
	v.bfOpt.OnConnectionChangeFunc(1, 0, net.IPv4(198, 51, 100, 1))
	v.bfOpt.OnConnectionChangeFunc(1, 1, net.IPv4(198, 51, 100, 2))
	v.bfOpt.OnConnectionChangeFunc(-1, 0, nil)
	v.bfOpt.OnUpstreamChunkFunc(100, 0)
	v.bfOpt.OnDownstreamChunkFunc(250, 1)
	assert.Equal(t, statsSnapshot{active: 1, sessions: 2, up: 100, down: 250}, v.stats.snapshot())
}

// The service is registered under lantern-box's registries, so a config's services entry builds
// a Volunteer.
func TestVolunteerParsesAsAService(t *testing.T) {
	ctx := newVolunteerBoxContext(t)
	opts, err := json.UnmarshalExtendedContext[sbOption.Options](ctx, []byte(`{
		"services": [{
			"type": "unbounded",
			"tag": "volunteer",
			"discovery_srv": "https://freddie.example",
			"egress_addr": "wss://egress.example",
			"stun_servers": ["stun:192.0.2.1:3478"],
			"max_peers": 25
		}]
	}`))
	require.NoError(t, err)
	require.Len(t, opts.Services, 1)
	got, ok := opts.Services[0].Options.(*option.UnboundedVolunteerOptions)
	require.True(t, ok, "services entry parsed as %T", opts.Services[0].Options)
	assert.Equal(t, 25, got.MaxPeers)
}

func newVolunteerBoxContext(t *testing.T) context.Context {
	t.Helper()
	ctx := include.Context(context.Background())
	reg, ok := service.FromContext[adapter.ServiceRegistry](ctx).(*boxService.Registry)
	require.True(t, ok)
	RegisterService(reg)
	return ctx
}

// A box started from a config with the volunteer service checks its STUN servers and then asks
// freddie for an offer. freddie's address is a hostname, as in production, so the signaling dialer
// must resolve it.
func TestVolunteerServiceSendsGenesis(t *testing.T) {
	stunAddr := startTestSTUNServer(t)
	genesis := make(chan url.Values, 8)
	freddie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil && r.Form.Get("send-to") == "genesis" {
			select {
			case genesis <- r.PostForm:
			default:
			}
		}
		// No consumer is waiting: answer the long poll after a pause, as freddie would.
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(freddie.Close)
	freddieURL, err := url.Parse(freddie.URL)
	require.NoError(t, err)
	freddieURL.Host = net.JoinHostPort("localhost", freddieURL.Port())

	ctx := newVolunteerBoxContext(t)
	// Logged at info, as a node logs by default, so the service's start-up log line is formatted.
	opts, err := json.UnmarshalExtendedContext[sbOption.Options](ctx, []byte(fmt.Sprintf(`{
		"log": {"level": "info", "output": %q},
		"dns": {"strategy": "ipv4_only"},
		"services": [{
			"type": "unbounded",
			"tag": "volunteer",
			"discovery_srv": %q,
			"egress_addr": "ws://127.0.0.1:1",
			"stun_servers": [%q],
			"max_peers": 1,
			"bandwidth_limit": "20mbit",
			"stats_interval": -1,
			"target_countries": ["CN", "IR"]
		}]
	}`, filepath.Join(t.TempDir(), "box.log"), freddieURL.String(), "stun:"+stunAddr)))
	require.NoError(t, err)
	b, err := sbox.New(sbox.Options{Context: ctx, Options: opts})
	require.NoError(t, err)
	// Close in cleanup, so the volunteer's workers stop even when an assertion below fails.
	t.Cleanup(func() {
		closed := make(chan error, 1)
		go func() { closed <- b.Close() }()
		select {
		case err := <-closed:
			assert.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Error("closing the box hung")
		}
	})
	require.NoError(t, b.Start())

	select {
	case form := <-genesis:
		assert.True(t, form.Has(common.PreflightVPNField),
			"the first genesis after a passed STUN check reports it")
	case <-time.After(15 * time.Second):
		t.Fatal("the volunteer never sent genesis")
	}
}

// startTestSTUNServer answers STUN binding requests on a loopback UDP port.
func startTestSTUNServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if req.Decode() != nil {
				continue
			}
			res := stun.MustBuild(req, stun.BindingSuccess, &stun.XORMappedAddress{IP: from.IP, Port: from.Port})
			_, _ = conn.WriteToUDP(res.Raw, from)
		}
	}()
	return conn.LocalAddr().String()
}
