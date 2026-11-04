package unbounded

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	sbAdapter "github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/portforward"
)

const testPublicIP = "203.0.113.7"

// fakeMapper is a gateway that maps whatever it is asked to, unless told
// otherwise.
type fakeMapper struct {
	externalIP   string
	externalErr  error
	portOffset   uint16
	mapErr       error
	method       string
	mapped       atomic.Int32
	renewals     atomic.Int32
	unmapped     atomic.Int32
	unmappedOnce chan struct{}
	once         sync.Once
	// onUnmap, if set, runs as UnmapPort starts.
	onUnmap func()
}

func newFakeMapper() *fakeMapper {
	return &fakeMapper{externalIP: testPublicIP, method: "fake", unmappedOnce: make(chan struct{})}
}

func (f *fakeMapper) MapPort(_ context.Context, protocol string, internalPort uint16, _ string) (*portforward.Mapping, error) {
	if protocol != portforward.UDP {
		return nil, errors.New("not UDP")
	}
	if f.mapErr != nil {
		return nil, f.mapErr
	}
	f.mapped.Add(1)
	return &portforward.Mapping{
		ExternalPort: internalPort + f.portOffset,
		InternalPort: internalPort,
		Protocol:     protocol,
		Method:       f.method,
	}, nil
}

func (f *fakeMapper) StartRenewal(context.Context) { f.renewals.Add(1) }

func (f *fakeMapper) UnmapPort(context.Context) error {
	if f.onUnmap != nil {
		f.onUnmap()
	}
	f.unmapped.Add(1)
	f.once.Do(func() { close(f.unmappedOnce) })
	return nil
}

func (f *fakeMapper) ExternalIP(context.Context) (string, error) {
	return f.externalIP, f.externalErr
}

// closeTrackingConn records whether the mux closed its socket.
type closeTrackingConn struct {
	net.PacketConn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.PacketConn.Close()
}

type testPool struct {
	*mappedPortPool
	clock        time.Time
	gatewayCalls atomic.Int32
	listenPorts  chan uint16
	mu           sync.Mutex
	conns        []*closeTrackingConn
}

// newTestPool returns a pool on loopback sockets whose gateway is mapper. No
// real router, STUN server or physical interface is touched.
func newTestPool(t *testing.T, mapper func(ctx context.Context) (portMapper, error)) *testPool {
	t.Helper()
	tp := &testPool{mappedPortPool: newMappedPortPool(), clock: time.Unix(1_000_000, 0), listenPorts: make(chan uint16, 16)}
	tp.now = func() time.Time {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		return tp.clock
	}
	tp.listen = func(_ context.Context, port uint16) (net.PacketConn, error) {
		tp.listenPorts <- port
		pc, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
		if err != nil {
			return nil, err
		}
		c := &closeTrackingConn{PacketConn: pc}
		tp.mu.Lock()
		tp.conns = append(tp.conns, c)
		tp.mu.Unlock()
		return c, nil
	}
	tp.gateway = func(ctx context.Context) (portMapper, error) {
		tp.gatewayCalls.Add(1)
		return mapper(ctx)
	}
	tp.manual = func(uint16) portMapper {
		t.Error("manual forwarder used without a manual port")
		return newFakeMapper()
	}
	tp.stunIP = func(context.Context, net.PacketConn, []string) (string, error) {
		t.Error("STUN used although the gateway reported an address")
		return "", errors.New("unexpected")
	}
	t.Cleanup(func() {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		for _, c := range tp.conns {
			_ = c.Close()
		}
	})
	return tp
}

func (tp *testPool) advance(d time.Duration) {
	tp.mu.Lock()
	tp.clock = tp.clock.Add(d)
	tp.mu.Unlock()
}

func (tp *testPool) conn(i int) *closeTrackingConn {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.conns[i]
}

func TestMappedPort_ConcurrentAcquirersShareOneMapping(t *testing.T) {
	fm := newFakeMapper()
	release := make(chan struct{})
	tp := newTestPool(t, func(ctx context.Context) (portMapper, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fm, nil
	})

	const n = 8
	results := make([]*mappedPort, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = tp.acquire(context.Background(), mappedPortRequest{})
		}()
	}
	// Let every acquirer reach the pool before discovery finishes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	require.NotNil(t, results[0])
	for _, m := range results {
		assert.Same(t, results[0], m, "every acquirer gets the same mapping")
	}
	assert.EqualValues(t, 1, tp.gatewayCalls.Load(), "one gateway discovery")
	assert.EqualValues(t, 1, fm.mapped.Load(), "one port mapping")
	assert.Equal(t, testPublicIP, results[0].externalIP)
	assert.NotNil(t, results[0].mux)
	assert.EqualValues(t, 1, fm.renewals.Load())
}

func TestMappedPort_LastReleaseUnmapsAndCloses(t *testing.T) {
	fm := newFakeMapper()
	tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })

	a := tp.acquire(context.Background(), mappedPortRequest{})
	b := tp.acquire(context.Background(), mappedPortRequest{})
	require.NotNil(t, a)
	require.Same(t, a, b)

	tp.release(a)
	assert.Zero(t, fm.unmapped.Load(), "another holder still uses the mapping")
	assert.False(t, tp.conn(0).closed.Load(), "another holder still uses the socket")

	var closedAtUnmap atomic.Bool
	fm.onUnmap = func() { closedAtUnmap.Store(tp.conn(0).closed.Load()) }
	tp.release(b)
	assert.EqualValues(t, 1, fm.unmapped.Load(), "the last release removes the mapping")
	assert.False(t, closedAtUnmap.Load(), "the socket holds the port until the mapping is gone")
	assert.True(t, tp.conn(0).closed.Load(), "the last release closes the socket")

	c := tp.acquire(context.Background(), mappedPortRequest{})
	require.NotNil(t, c)
	assert.NotSame(t, a, c, "a released mapping is not handed out again")
	assert.EqualValues(t, 2, tp.gatewayCalls.Load())
	tp.release(c)
}

func TestMappedPort_FailureIsCachedThenRetried(t *testing.T) {
	tp := newTestPool(t, func(context.Context) (portMapper, error) { return nil, portforward.ErrNoPortForwarding })

	assert.Nil(t, tp.acquire(context.Background(), mappedPortRequest{}))
	assert.EqualValues(t, 1, tp.gatewayCalls.Load())

	tp.advance(mappedPortRetryAfter - time.Second)
	assert.Nil(t, tp.acquire(context.Background(), mappedPortRequest{}))
	assert.EqualValues(t, 1, tp.gatewayCalls.Load(), "no retry inside the window")

	tp.advance(2 * time.Second)
	assert.Nil(t, tp.acquire(context.Background(), mappedPortRequest{}))
	assert.EqualValues(t, 2, tp.gatewayCalls.Load(), "retried once the window ends")
}

func TestMappedPort_WaiterBoundedByItsContext(t *testing.T) {
	release := make(chan struct{})
	tp := newTestPool(t, func(ctx context.Context) (portMapper, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newFakeMapper(), nil
	})
	leader := make(chan *mappedPort)
	go func() { leader <- tp.acquire(context.Background(), mappedPortRequest{}) }()
	<-tp.listenPorts

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	assert.Nil(t, tp.acquire(ctx, mappedPortRequest{}), "a waiter whose ctx ends gets no mapping")
	assert.Less(t, time.Since(started), time.Second)

	close(release)
	m := <-leader
	require.NotNil(t, m)
	assert.EqualValues(t, 1, tp.gatewayCalls.Load(), "the waiter started no attempt of its own")
	tp.release(m)
}

func TestMappedPort_ManualPortSkipsDiscovery(t *testing.T) {
	tp := newTestPool(t, func(context.Context) (portMapper, error) {
		t.Error("gateway discovery used despite a manual port")
		return nil, errors.New("unexpected")
	})
	port := freeUDPPort(t)
	var manualPort uint16
	manual := newFakeMapper()
	manual.externalIP = ""
	manual.method = "manual"
	tp.manual = func(p uint16) portMapper {
		manualPort = p
		return manual
	}
	var stunServers []string
	tp.stunIP = func(_ context.Context, _ net.PacketConn, servers []string) (string, error) {
		stunServers = servers
		return testPublicIP, nil
	}

	m := tp.acquire(context.Background(), mappedPortRequest{manualPort: port, stunServers: []string{"stun:example.com:3478"}})
	require.NotNil(t, m)
	assert.Equal(t, port, <-tp.listenPorts, "the socket is bound on the manual port")
	assert.Equal(t, port, manualPort)
	assert.Equal(t, port, m.port)
	assert.Equal(t, []string{"stun:example.com:3478"}, stunServers, "a manual forward has no gateway to ask for the address")
	assert.Equal(t, testPublicIP, m.externalIP)
	assert.Zero(t, tp.gatewayCalls.Load())
	tp.release(m)
}

func TestMappedPort_UnusableMappingYieldsNone(t *testing.T) {
	for name, tc := range map[string]struct {
		ip         string
		portOffset uint16
	}{
		"private":          {ip: "192.168.1.20"},
		"private 10/8":     {ip: "10.1.2.3"},
		"cgnat":            {ip: "100.72.1.1"},
		"loopback":         {ip: "127.0.0.1"},
		"link-local":       {ip: "169.254.10.1"},
		"unspecified":      {ip: "0.0.0.0"},
		"ipv6":             {ip: "2606:4700::1111"},
		"garbage":          {ip: "not-an-ip"},
		"changed ext port": {ip: testPublicIP, portOffset: 1},
	} {
		t.Run(name, func(t *testing.T) {
			fm := newFakeMapper()
			fm.externalIP = tc.ip
			fm.portOffset = tc.portOffset
			tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })
			var closedAtUnmap atomic.Bool
			fm.onUnmap = func() { closedAtUnmap.Store(tp.conn(0).closed.Load()) }

			assert.Nil(t, tp.acquire(context.Background(), mappedPortRequest{}))
			select {
			case <-fm.unmappedOnce:
			case <-time.After(5 * time.Second):
				t.Fatal("the mapping was not removed")
			}
			assert.False(t, closedAtUnmap.Load(), "the socket holds the port until the mapping is gone")
			assert.Eventually(t, tp.conn(0).closed.Load, 5*time.Second, 10*time.Millisecond, "the socket is closed")
			assert.Zero(t, fm.renewals.Load(), "no renewal for a mapping that isn't used")
		})
	}
	// The control: the same setup with a public address does yield a mapping.
	fm := newFakeMapper()
	tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })
	m := tp.acquire(context.Background(), mappedPortRequest{})
	require.NotNil(t, m)
	tp.release(m)
}

func TestIsPublicIPv4(t *testing.T) {
	assert.True(t, isPublicIPv4("8.8.8.8"))
	assert.True(t, isPublicIPv4("::ffff:8.8.8.8"))
	assert.True(t, isPublicIPv4("100.128.0.1"), "just past CGNAT")
	assert.False(t, isPublicIPv4("100.127.255.255"))
	assert.False(t, isPublicIPv4("172.16.0.1"))
	assert.False(t, isPublicIPv4("224.0.0.1"))
	assert.False(t, isPublicIPv4("255.255.255.255"))
	assert.False(t, isPublicIPv4(""))
}

// newMappingTestOutbound builds an outbound on ctx and, if ctx enabled port
// mapping, points it at tp instead of the process-wide pool.
func newMappingTestOutbound(t *testing.T, ctx context.Context, tp *testPool) *Outbound {
	t.Helper()
	out, err := NewOutbound(ctx, nil, nil, "unbounded", option.UnboundedOutboundOptions{STUNServers: []string{"stun:a:1"}})
	require.NoError(t, err)
	o := out.(*Outbound)
	if o.mappedPorts != nil {
		require.Same(t, sharedMappedPorts, o.mappedPorts, "outbounds share the process-wide mapping")
		o.mappedPorts = tp.mappedPortPool
	}
	return o
}

func TestOutbound_MappedPort(t *testing.T) {
	t.Run("off without the context value", func(t *testing.T) {
		tp := newTestPool(t, func(context.Context) (portMapper, error) {
			t.Error("gateway reached without the embedder enabling mapping")
			return newFakeMapper(), nil
		})
		o := newMappingTestOutbound(t, context.Background(), tp)
		assert.Nil(t, o.mappedPorts, "the outbound holds no pool")

		o.acquireMappedPort()
		assert.Zero(t, tp.gatewayCalls.Load())
		assert.Nil(t, o.rtcOpt.ICEUDPMux)
		assert.Empty(t, o.rtcOpt.ICEUDPMuxExternalIP)
		require.NoError(t, o.Close())
	})

	t.Run("failure leaves rtcOpt unset", func(t *testing.T) {
		tp := newTestPool(t, func(context.Context) (portMapper, error) { return nil, portforward.ErrNoPortForwarding })
		o := newMappingTestOutbound(t, lbAdapter.ContextWithUnboundedPortMapping(context.Background(), 0), tp)

		o.acquireMappedPort()
		assert.EqualValues(t, 1, tp.gatewayCalls.Load(), "the outbound tried")
		assert.Nil(t, o.rtcOpt.ICEUDPMux)
		assert.Empty(t, o.rtcOpt.ICEUDPMuxExternalIP)
		require.NoError(t, o.Close())
	})

	t.Run("port 0 asks the gateway, and Close releases", func(t *testing.T) {
		fm := newFakeMapper()
		tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })
		o := newMappingTestOutbound(t, lbAdapter.ContextWithUnboundedPortMapping(context.Background(), 0), tp)

		o.acquireMappedPort()
		assert.EqualValues(t, 1, tp.gatewayCalls.Load())
		require.NotNil(t, o.rtcOpt.ICEUDPMux)
		assert.Equal(t, testPublicIP, o.rtcOpt.ICEUDPMuxExternalIP)

		require.NoError(t, o.Close())
		assert.EqualValues(t, 1, fm.unmapped.Load(), "Close releases the outbound's hold")
		assert.True(t, tp.conn(0).closed.Load())
		require.NoError(t, o.Close(), "a second Close releases nothing more")
		assert.EqualValues(t, 1, fm.unmapped.Load())
	})

	t.Run("a manual port uses the manual forwarder", func(t *testing.T) {
		tp := newTestPool(t, func(context.Context) (portMapper, error) {
			t.Error("gateway discovery used despite a manual port")
			return nil, errors.New("unexpected")
		})
		port := freeUDPPort(t)
		manual := newFakeMapper()
		manual.externalIP = ""
		var manualPort uint16
		tp.manual = func(p uint16) portMapper {
			manualPort = p
			return manual
		}
		tp.stunIP = func(context.Context, net.PacketConn, []string) (string, error) { return testPublicIP, nil }
		o := newMappingTestOutbound(t, lbAdapter.ContextWithUnboundedPortMapping(context.Background(), port), tp)

		o.acquireMappedPort()
		assert.Equal(t, port, manualPort)
		assert.Equal(t, port, <-tp.listenPorts)
		require.NotNil(t, o.rtcOpt.ICEUDPMux)
		assert.Zero(t, tp.gatewayCalls.Load())
		require.NoError(t, o.Close())
	})
}

func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	require.NoError(t, pc.Close())
	return port
}

func TestStunExternalIP(t *testing.T) {
	server, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer server.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if req.Decode() != nil {
				continue
			}
			// A stray packet first: the forwarded port takes anything.
			_, _ = server.WriteTo([]byte("not stun"), from)
			res := stun.MustBuild(req, stun.BindingSuccess,
				&stun.XORMappedAddress{IP: net.ParseIP(testPublicIP), Port: 4242})
			_, _ = server.WriteTo(res.Raw, from)
		}
	}()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ip, err := stunExternalIP(ctx, conn, []string{"stun:" + server.LocalAddr().String()})
	require.NoError(t, err)
	assert.Equal(t, testPublicIP, ip)

	// The deadline is cleared for the mux, which reads conn next.
	go func() {
		time.Sleep(stunQueryTimeout + 200*time.Millisecond)
		_, _ = server.WriteTo([]byte("late"), conn.LocalAddr())
	}()
	buf := make([]byte, 16)
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err, "a read after the query must not hit the query's deadline")
	assert.Equal(t, "late", string(buf[:n]))
}

func TestMappedPort_LocalIPFailureIsItsOwnStage(t *testing.T) {
	fm := newFakeMapper()
	fm.mapErr = fmt.Errorf("determine local ip: %w", portforward.ErrNoLocalIP)
	tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })

	m, stage, err := tp.establish(context.Background(), mappedPortRequest{})
	assert.Nil(t, m)
	require.ErrorIs(t, err, portforward.ErrNoLocalIP)
	assert.Equal(t, "local_ip", stage)
	assert.True(t, tp.conn(0).closed.Load(), "the socket is closed")

	fm.mapErr = portforward.ErrNoPortForwarding
	_, stage, _ = tp.establish(context.Background(), mappedPortRequest{})
	assert.Equal(t, "map", stage, "other mapping failures stay under map")
}

// broflake's workers read rtcOpt's mux fields as they build peer connections,
// and can outlive ui.Stop, so Close must not write them. Run with -race. The
// overlap is a matter of timing, so it is tried several times.
func TestOutbound_CloseLeavesRTCOptForLingeringWorkers(t *testing.T) {
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(discovery.Close)

	for range 8 {
		fm := newFakeMapper()
		tp := newTestPool(t, func(context.Context) (portMapper, error) { return fm, nil })
		ctx := lbAdapter.ContextWithUnboundedPortMapping(context.Background(), 0)
		out, err := NewOutbound(ctx, nil, sblog.NewNOPFactory().Logger(), "unbounded", option.UnboundedOutboundOptions{
			DiscoverySrv:                  discovery.URL,
			InsecureDoNotVerifyClientCert: true,
		})
		require.NoError(t, err)
		o := out.(*Outbound)
		o.mappedPorts = tp.mappedPortPool
		require.NoError(t, o.Start(sbAdapter.StartStatePostStart))
		require.NotNil(t, o.rtcOpt.ICEUDPMux, "Start took the mapping")

		// At once, while the consumer's workers are still building their first
		// peer connections.
		require.NoError(t, o.Close())
		assert.EqualValues(t, 1, fm.unmapped.Load(), "Close released the mapping")
		// Workers may still run after Stop returns.
		time.Sleep(100 * time.Millisecond)
	}
}

// The mapped socket is dual-stack: pion gathers host candidates only on the
// mux, so an IPv4-only socket would cost every IPv6 host candidate.
func TestListenMappedUDP_DualStack(t *testing.T) {
	v6, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 on this host")
	}
	v6.Close()
	pc, err := listenMappedUDP(context.Background(), 0)
	require.NoError(t, err)
	defer pc.Close()
	addr, ok := pc.LocalAddr().(*net.UDPAddr)
	require.True(t, ok)
	assert.Nil(t, addr.IP.To4(), "bound to %v, want the IPv6 wildcard", addr.IP)
	assert.True(t, addr.IP.IsUnspecified(), "bound to %v, want the IPv6 wildcard", addr.IP)
}

// manualTestPool is a pool whose manual forwarder is fm and whose STUN lookup
// is stun, on a free port.
func manualTestPool(t *testing.T, fm *fakeMapper, stun func(context.Context) (string, error)) (*testPool, uint16) {
	t.Helper()
	tp := newTestPool(t, func(context.Context) (portMapper, error) {
		t.Error("gateway discovery used despite a manual port")
		return nil, errors.New("unexpected")
	})
	fm.externalIP = ""
	tp.manual = func(uint16) portMapper { return fm }
	tp.stunIP = func(ctx context.Context, _ net.PacketConn, _ []string) (string, error) { return stun(ctx) }
	return tp, freeUDPPort(t)
}

// A manual-port attempt cut short by its outbound closing frees the port before
// waking the next acquirer, which can then bind it.
func TestMappedPort_CanceledManualAttemptFreesPort(t *testing.T) {
	fm := newFakeMapper()
	var calls atomic.Int32
	tp, port := manualTestPool(t, fm, func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return testPublicIP, nil
	})
	req := mappedPortRequest{manualPort: port, stunServers: []string{"stun:example.com:3478"}}

	parent, cancel := context.WithCancel(context.Background())
	first := make(chan *mappedPort, 1)
	go func() { first <- tp.acquire(parent, req) }()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, time.Millisecond)
	second := make(chan *mappedPort, 1)
	go func() { second <- tp.acquire(context.Background(), req) }()
	cancel()

	assert.Nil(t, <-first)
	m := <-second
	require.NotNil(t, m, "the waiting acquirer could not bind the port the canceled attempt held")
	tp.release(m)
}

// An acquirer that comes while the last release is still removing the mapping
// waits for its socket to close, rather than fail to bind the manual port and
// remember that failure.
func TestMappedPort_AcquireWaitsForRetiringMapping(t *testing.T) {
	fm := newFakeMapper()
	tp, port := manualTestPool(t, fm, func(context.Context) (string, error) { return testPublicIP, nil })
	req := mappedPortRequest{manualPort: port, stunServers: []string{"stun:example.com:3478"}}

	m := tp.acquire(context.Background(), req)
	require.NotNil(t, m)
	unmapping, proceed := make(chan struct{}), make(chan struct{})
	fm.onUnmap = func() {
		close(unmapping)
		<-proceed
	}
	go tp.release(m)
	<-unmapping

	next := make(chan *mappedPort, 1)
	go func() { next <- tp.acquire(context.Background(), req) }()
	time.Sleep(50 * time.Millisecond)
	fm.onUnmap = nil
	close(proceed)

	n := <-next
	require.NotNil(t, n, "the acquirer failed while the old mapping was retiring")
	assert.NotSame(t, m, n)
	tp.release(n)
}
