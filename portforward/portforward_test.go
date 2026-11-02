package portforward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIGD struct {
	mu          sync.Mutex
	addCalls    atomic.Int64
	deleteCalls atomic.Int64
	addErr      error
	deleteErr   error
	extIPErr    error
	extIP       string
	addBlock    chan struct{} // if non-nil, AddPortMapping blocks on receive
	deleteBlock chan struct{} // if non-nil, DeletePortMapping blocks on receive, after recording
	lastAdd     mappingArgs
	lastDelete  deleteArgs
	// adds and deletes are the protocol of every call, in order.
	adds, deletes []string
	// deleteErrFor, if set, overrides deleteErr per protocol.
	deleteErrFor map[string]error
}

type mappingArgs struct {
	externalPort, internalPort            uint16
	protocol, internalClient, description string
	leaseDuration                         uint32
}

type deleteArgs struct {
	externalPort uint16
	protocol     string
}

func (f *fakeIGD) AddPortMapping(_ string, externalPort uint16, protocol string, internalPort uint16, internalClient string, _ bool, description string, leaseDuration uint32) error {
	f.addCalls.Add(1)
	if f.addBlock != nil {
		<-f.addBlock
	}
	f.mu.Lock()
	f.adds = append(f.adds, protocol)
	f.lastAdd = mappingArgs{
		externalPort:   externalPort,
		internalPort:   internalPort,
		protocol:       protocol,
		internalClient: internalClient,
		description:    description,
		leaseDuration:  leaseDuration,
	}
	f.mu.Unlock()
	return f.addErr
}

func (f *fakeIGD) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	// Recorded before it is counted, so a test that sees the count also sees
	// the delete.
	f.mu.Lock()
	f.lastDelete = deleteArgs{externalPort: externalPort, protocol: protocol}
	f.deletes = append(f.deletes, protocol)
	err := f.deleteErr
	if f.deleteErrFor != nil {
		err = f.deleteErrFor[protocol]
	}
	f.mu.Unlock()
	f.deleteCalls.Add(1)
	if f.deleteBlock != nil {
		<-f.deleteBlock
	}
	return err
}

func (f *fakeIGD) GetExternalIPAddress() (string, error) {
	if f.extIPErr != nil {
		return "", f.extIPErr
	}
	if f.extIP == "" {
		return "203.0.113.1", nil
	}
	return f.extIP, nil
}

func newTestForwarder(t *testing.T, c *fakeIGD) *Forwarder {
	t.Helper()
	return &Forwarder{client: c, method: "fake"}
}

func TestForwarder_MapPort_HappyPath(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	m, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)
	assert.Equal(t, uint16(30001), m.ExternalPort)
	assert.Equal(t, uint16(30001), m.InternalPort)
	assert.Equal(t, "TCP", m.Protocol)
	assert.Equal(t, "fake", m.Method)
	assert.Equal(t, int64(1), c.addCalls.Load())
}

func TestForwarder_MapPort_DoubleMapRejected(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)
	_, err = f.MapPort(context.Background(), TCP, 30002, "test")
	assert.ErrorContains(t, err, "already has an active TCP mapping")
}

func TestForwarder_MapPort_PropagatesGatewayError(t *testing.T) {
	c := &fakeIGD{addErr: errors.New("conflict")}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	assert.ErrorContains(t, err, "add port mapping")
}

// ProbeUPnP wraps NewForwarder and returns false on any error, including
// ctx cancellation / deadline expiration. A successful probe requires a
// real IGD on the test host's network, which CI doesn't have — but the
// negative-path contract (cancelled ctx → false within the cancel
// window, no leaked goroutines) is what callers actually depend on for
// timely UI feedback when UPnP is unavailable.
func TestProbeUPnP_CancelledContextReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	got := ProbeUPnP(ctx)
	elapsed := time.Since(start)

	assert.False(t, got, "cancelled ctx must yield false")
	assert.Less(t, elapsed, 2*time.Second, "probe should bail fast on a cancelled ctx, not wait for M-SEARCH")
}

// MapPort must respect the caller's context — a hung router shouldn't tie up
// Start past its deadline.
func TestForwarder_MapPort_RespectsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	c := &fakeIGD{addBlock: block}
	f := newTestForwarder(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.MapPort(ctx, TCP, 30001, "test")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	close(block) // release the leaked goroutine
}

func TestForwarder_UnmapPort_NoMappingIsNoop(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, int64(0), c.deleteCalls.Load())
}

func TestForwarder_UnmapPort_RemovesMapping(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)

	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, int64(1), c.deleteCalls.Load())
	assert.Equal(t, uint16(30001), c.lastDelete.externalPort)
	assert.Equal(t, "TCP", c.lastDelete.protocol)

	// Calling MapPort after UnmapPort must succeed (mapping cleared).
	_, err = f.MapPort(context.Background(), TCP, 30002, "test")
	require.NoError(t, err)
}

func TestForwarder_StartRenewal_ReissuesAddPortMapping(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	// Use a short lease so the renewal interval clamps to the 1m floor; we
	// invoke the loop directly with a fast interval to avoid waiting.
	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go f.renewLoop(ctx, 20*time.Millisecond)

	deadline := time.After(2 * time.Second)
	for c.addCalls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("renewal fired only %d times", c.addCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
}

// Cancelling the renewal ctx must stop the loop even with a long interval.
func TestForwarder_StartRenewal_CancelsCleanly(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.renewLoop(ctx, time.Hour)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewLoop did not exit after ctx cancel")
	}
}

func TestForwarder_ExternalIP(t *testing.T) {
	c := &fakeIGD{extIP: "203.0.113.50"}
	f := newTestForwarder(t, c)
	ip, err := f.ExternalIP(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.50", ip)
}

func TestForwarder_ExternalIP_EmptyIsError(t *testing.T) {
	f := &Forwarder{client: emptyExtIPClient{}, method: "fake"}
	_, err := f.ExternalIP(context.Background())
	assert.ErrorContains(t, err, "empty external ip")
}

type emptyExtIPClient struct{}

func (emptyExtIPClient) AddPortMapping(string, uint16, string, uint16, string, bool, string, uint32) error {
	return nil
}
func (emptyExtIPClient) DeletePortMapping(string, uint16, string) error { return nil }
func (emptyExtIPClient) GetExternalIPAddress() (string, error)          { return "", nil }

func TestForwarder_ExternalIP_PropagatesError(t *testing.T) {
	c := &fakeIGD{extIPErr: errors.New("upstream timeout")}
	f := newTestForwarder(t, c)
	_, err := f.ExternalIP(context.Background())
	assert.ErrorContains(t, err, "upstream timeout")
}

func TestLocalIP(t *testing.T) {
	// Best-effort: localIP needs working UDP. CI machines have it; offline
	// dev machines may not. Skip rather than fail if it errors.
	ip, err := LocalIP()
	if err != nil {
		t.Skipf("localIP unavailable in this environment: %v", err)
	}
	assert.NotEmpty(t, ip)
}

// The interface-scan fallback covers networks where the UDP-noop trick
// fails (IPv6-only host, kernel rejects 8.8.8.8, etc.). Skip if the dev
// machine genuinely lacks a private IPv4 — running this on a CI worker
// without a LAN address shouldn't fail the build.
func TestLocalIPByInterfaceScan(t *testing.T) {
	ip, err := localIPByInterfaceScan()
	if err != nil {
		t.Skipf("no private ipv4 interface available: %v", err)
	}
	assert.NotEmpty(t, ip)
}

// MapPort's gateway-refused path must surface ErrNoPortForwarding via
// errors.Is so callers can distinguish "this network won't work" from
// "something else broke", per the package-level docstring.
func TestForwarder_MapPort_GatewayErrorWrapsErrNoPortForwarding(t *testing.T) {
	c := &fakeIGD{addErr: errors.New("ConflictInMappingEntry")}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoPortForwarding, "callers must be able to detect via errors.Is")
	assert.ErrorContains(t, err, "ConflictInMappingEntry", "underlying gateway error must survive for diagnostics")
}

// A renewal that was already mid-call when UnmapPort deleted the mapping
// re-adds an inbound forward to this host, and nothing else would ever remove
// it — permanent on routers that ignore the requested lease. The renewal must
// notice the teardown and delete what it re-added.
func TestForwarder_RenewalRacingTeardown_DeletesWhatItReAdded(t *testing.T) {
	release := make(chan struct{})
	c := &fakeIGD{addBlock: release}
	f := newTestForwarder(t, c)
	f.mappings = map[string]*Mapping{TCP: {
		ExternalPort: 15000, InternalPort: 15000, InternalIP: "192.168.1.10",
		Protocol: TCP, LeaseDuration: time.Hour,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	loopDone := make(chan struct{})
	go func() { f.renewLoop(ctx, time.Millisecond); close(loopDone) }()

	// Park the renewal inside AddPortMapping, past its teardown pre-check.
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 },
		2*time.Second, time.Millisecond, "renewal never reached AddPortMapping")

	require.NoError(t, f.UnmapPort(context.Background()))
	deletesAfterUnmap := c.deleteCalls.Load()
	require.Equal(t, int64(1), deletesAfterUnmap, "UnmapPort should have deleted once")

	// Let the in-flight renewal complete — it lands after the delete.
	close(release)

	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("renewLoop did not exit after teardown")
	}
	assert.Eventually(t, func() bool { return c.deleteCalls.Load() == 2 }, 2*time.Second, time.Millisecond,
		"the renewal must delete the mapping it re-added after teardown")
	c.mu.Lock()
	assert.Equal(t, uint16(15000), c.lastDelete.externalPort)
	c.mu.Unlock()
}

// Once teardown has run, a later tick must not touch the gateway at all.
func TestForwarder_RenewalAfterTeardown_DoesNotReAdd(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	f.mappings = map[string]*Mapping{TCP: {
		ExternalPort: 15001, InternalPort: 15001, InternalIP: "192.168.1.10",
		Protocol: TCP, LeaseDuration: time.Hour,
	}}
	require.NoError(t, f.UnmapPort(context.Background()))
	require.Equal(t, int64(1), c.deleteCalls.Load())

	// renewLoop started (or ticking) after teardown must exit without adding.
	done := make(chan struct{})
	go func() { f.renewLoop(context.Background(), time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("renewLoop did not exit once the mapping was torn down")
	}
	assert.Equal(t, int64(0), c.addCalls.Load(),
		"no AddPortMapping may be issued after UnmapPort")
	assert.Equal(t, int64(1), c.deleteCalls.Load(), "no compensating delete was needed")
}

// A caller that gives up mid-MapPort must not leave a mapping behind: the
// gateway may still accept it, and because f.mapping was never recorded,
// UnmapPort would short-circuit and nothing would ever remove the forward.
func TestForwarder_MapPort_CancelledMidCall_RemovesAcceptedMapping(t *testing.T) {
	release := make(chan struct{})
	c := &fakeIGD{addBlock: release}
	f := newTestForwarder(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		m   *Mapping
		err error
	}
	res := make(chan result, 1)
	go func() {
		m, err := f.MapPort(ctx, TCP, 15100, "test")
		res <- result{m, err}
	}()

	// Park inside AddPortMapping, then give up.
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 },
		2*time.Second, time.Millisecond, "MapPort never reached AddPortMapping")
	cancel()
	close(release) // the gateway accepts it anyway

	var got result
	select {
	case got = <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("MapPort did not return")
	}

	require.Error(t, got.err, "a cancelled caller must get an error")
	assert.ErrorIs(t, got.err, context.Canceled)
	assert.Nil(t, got.m)
	f.mu.Lock()
	assert.Empty(t, f.mappings, "no mapping should be recorded")
	f.mu.Unlock()
	require.Eventually(t, func() bool { return c.deleteCalls.Load() == 1 }, 2*time.Second, time.Millisecond,
		"the accepted-but-unreported mapping must be deleted, or it survives untracked")
	c.mu.Lock()
	assert.Equal(t, uint16(15100), c.lastDelete.externalPort)
	c.mu.Unlock()
}

// A caller that gives up gets MapPort back at once, without waiting on a slow
// gateway while holding the Forwarder, and the add's cleanup waits for the add
// to resolve.
func TestForwarder_MapPort_ReturnsWhenCancelledMidCall(t *testing.T) {
	release := make(chan struct{})
	c := &fakeIGD{addBlock: release}
	f := newTestForwarder(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := f.MapPort(ctx, TCP, 15200, "test")
		res <- err
	}()
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 },
		2*time.Second, time.Millisecond, "MapPort never reached AddPortMapping")
	cancel()
	select {
	case err := <-res:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("MapPort kept waiting on the gateway after its caller gave up")
	}
	// The Forwarder isn't held meanwhile.
	require.True(t, f.mu.TryLock(), "MapPort returned but left the Forwarder locked")
	f.mu.Unlock()
	assert.Equal(t, int64(0), c.deleteCalls.Load(), "no delete may be sent before the add resolves")
	close(release)
	require.Eventually(t, func() bool { return c.deleteCalls.Load() == 1 }, 2*time.Second, time.Millisecond)
}

// errOrAbsent keeps "discovery completed, nothing answered" from rendering as
// "<nil>", which reads as though no attempt was made.
func TestErrOrAbsent(t *testing.T) {
	assert.Equal(t, "boom", errOrAbsent(errors.New("boom"), nil))
	assert.Equal(t, "no gateway responded", errOrAbsent(nil, nil))
	assert.Equal(t, "ok", errOrAbsent(nil, &fakeIGD{}))
}

// A local timeout waiting for AddPortMapping is not a gateway refusing the
// mapping. The call never returned, so the router may well have created the
// forward — and nothing then tracks it: no Forwarder renews it, no UnmapPort
// removes it, and it can outlive the process entirely. Orphans accumulate
// across runs, which is a plausible mechanism for a gateway that later accepts
// AddPortMapping without honouring it once its table is full.
//
// The timeout branch has always claimed to handle this ("assume it landed so
// the cleanup below removes it"), but that cleanup is gated on ctx.Err(), so a
// timeout with a live context fell straight through to the generic error return
// and deleted nothing. renewLoop already defends against exactly this on its
// own timeouts.
func TestForwarder_MapPort_RemovesTheMappingWhenTheGatewayNeverAnswers(t *testing.T) {
	prev := addCallTimeout
	addCallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { addCallTimeout = prev })

	// Unblocked only after the assertions, so AddPortMapping is still in flight
	// for the whole test — the case where the outcome is genuinely unknowable.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	c := &fakeIGD{addBlock: block}
	f := newTestForwarder(t, c)

	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.Error(t, err)
	// Still the "this network can't host a peer" sentinel: the caller has no
	// mapping either way, and the retry decision is the same.
	assert.ErrorIs(t, err, ErrNoPortForwarding)
	assert.ErrorContains(t, err, "timed out")

	// The delete waits for the add to resolve: sent earlier, it could reach
	// the gateway ahead of the add.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), c.deleteCalls.Load(), "a delete was sent while the add was still in flight")
	block <- struct{}{}
	assert.Eventually(t, func() bool { return c.deleteCalls.Load() == 1 }, time.Second, 10*time.Millisecond,
		"a mapping the gateway may have created was left on the router")

	// And no mapping is retained locally, so a later UnmapPort has nothing
	// stale to delete and StartRenewal has nothing to renew.
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.mappings)
}

// A node maps TCP for its inbounds and UDP for WebRTC on one forwarder. Each
// protocol gets its own gateway rule, and a second mapping of the same
// protocol is still refused.
func TestForwarder_MapPort_OneMappingPerProtocol(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	tcp, err := f.MapPort(context.Background(), TCP, 30001, "tcp")
	require.NoError(t, err)
	udp, err := f.MapPort(context.Background(), UDP, 30002, "udp")
	require.NoError(t, err)
	assert.Equal(t, TCP, tcp.Protocol)
	assert.Equal(t, UDP, udp.Protocol)
	assert.Equal(t, uint16(30002), udp.ExternalPort)
	assert.Equal(t, []string{TCP, UDP}, c.adds)

	_, err = f.MapPort(context.Background(), UDP, 30003, "udp")
	assert.ErrorContains(t, err, "already has an active UDP mapping")
}

func TestForwarder_MapPort_RejectsUnknownProtocol(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), "SCTP", 30001, "test")
	assert.ErrorContains(t, err, "unsupported protocol")
	assert.Equal(t, int64(0), c.addCalls.Load())
}

func TestForwarder_UnmapPort_RemovesEveryProtocol(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), UDP, 30002, "udp")
	require.NoError(t, err)
	_, err = f.MapPort(context.Background(), TCP, 30001, "tcp")
	require.NoError(t, err)

	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, []string{TCP, UDP}, c.deletes)
	assert.Empty(t, f.mappings)
}

// A failed delete keeps that mapping so a retry can remove it, without holding
// up the other protocol's delete.
func TestForwarder_UnmapPort_KeepsOnlyWhatFailedToDelete(t *testing.T) {
	c := &fakeIGD{deleteErrFor: map[string]error{TCP: errors.New("gateway busy")}}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), TCP, 30001, "tcp")
	require.NoError(t, err)
	_, err = f.MapPort(context.Background(), UDP, 30002, "udp")
	require.NoError(t, err)

	err = f.UnmapPort(context.Background())
	assert.ErrorContains(t, err, "delete TCP port mapping")
	assert.ErrorContains(t, err, "gateway busy")
	assert.Equal(t, []string{TCP, UDP}, c.deletes)
	require.Len(t, f.mappings, 1)
	assert.NotNil(t, f.mappings[TCP])

	c.mu.Lock()
	c.deleteErrFor = nil
	c.mu.Unlock()
	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Empty(t, f.mappings)
}

// Renewal re-adds every protocol's mapping, including one added after it
// started, each with its own protocol and description.
func TestForwarder_Renewal_RenewsEveryProtocol(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), TCP, 30001, "tcp")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.renewLoop(ctx, 5*time.Millisecond); close(done) }()

	// Add UDP only once renewal has ticked with TCP alone, so a loop that
	// snapshots its mappings at startup fails.
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 2 },
		2*time.Second, time.Millisecond, "the TCP mapping was never renewed")
	_, err = f.MapPort(context.Background(), UDP, 30002, "udp")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.lastAdd.protocol == UDP && c.lastAdd.description == "udp (renew)"
	}, 2*time.Second, time.Millisecond, "the UDP mapping was never renewed")
	cancel()
	<-done
}

// A renewal racing teardown with two mappings stops at the first raced one and
// removes what it re-added, without renewing the rest.
func TestForwarder_RenewalRacingTeardown_StopsAcrossProtocols(t *testing.T) {
	release := make(chan struct{})
	c := &fakeIGD{addBlock: release}
	f := newTestForwarder(t, c)
	f.mappings = map[string]*Mapping{
		TCP: {ExternalPort: 15000, InternalPort: 15000, Protocol: TCP, LeaseDuration: time.Hour},
		UDP: {ExternalPort: 15001, InternalPort: 15001, Protocol: UDP, LeaseDuration: time.Hour},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	loopDone := make(chan struct{})
	go func() { f.renewLoop(ctx, time.Millisecond); close(loopDone) }()

	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 },
		2*time.Second, time.Millisecond, "renewal never reached AddPortMapping")
	require.NoError(t, f.UnmapPort(context.Background()))
	close(release)

	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("renewLoop did not exit after teardown")
	}
	assert.Equal(t, int64(1), c.addCalls.Load(), "no renewal may start after teardown")
	assert.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return slices.Equal(c.deletes, []string{TCP, UDP, TCP})
	}, 2*time.Second, time.Millisecond, "teardown deletes both, then the raced TCP renewal is undone")
}

// Cancelling renewal while one protocol's renewal is in flight lets that call
// finish but renews no other protocol.
func TestForwarder_RenewalCancelled_StopsBeforeNextProtocol(t *testing.T) {
	release := make(chan struct{})
	c := &fakeIGD{addBlock: release}
	f := newTestForwarder(t, c)
	f.mappings = map[string]*Mapping{
		TCP: {ExternalPort: 15000, InternalPort: 15000, Protocol: TCP, LeaseDuration: time.Hour},
		UDP: {ExternalPort: 15001, InternalPort: 15001, Protocol: UDP, LeaseDuration: time.Hour},
	}
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() { f.renewLoop(ctx, time.Millisecond); close(loopDone) }()

	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 },
		2*time.Second, time.Millisecond, "renewal never reached AddPortMapping")
	cancel()
	close(release)

	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("renewLoop did not exit after cancel")
	}
	assert.Equal(t, []string{TCP}, c.adds, "the UDP mapping was renewed after renewal was cancelled")
}

// A Forwarder that maps again after UnmapPort renews its new mapping.
func TestForwarder_RemapAfterUnmap_Renews(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)
	require.NoError(t, f.UnmapPort(context.Background()))
	_, err = f.MapPort(context.Background(), TCP, 30002, "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.renewLoop(ctx, 5*time.Millisecond)
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 4 }, 2*time.Second, time.Millisecond,
		"the mapping made after UnmapPort was never renewed")
	c.mu.Lock()
	assert.Equal(t, uint16(30002), c.lastAdd.externalPort)
	c.mu.Unlock()
}

// A port whose abandoned add is still being cleaned up is refused by MapPort
// until the cleanup's delete has returned, so the cleanup can't remove a new
// mapping of the same port.
func TestForwarder_SettlingPortIsRefusedUntilCleanedUp(t *testing.T) {
	release := make(chan struct{})
	deleteDone := make(chan struct{})
	c := &fakeIGD{addBlock: release, deleteBlock: deleteDone}
	f := newTestForwarder(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		_, err := f.MapPort(ctx, UDP, 41641, "test")
		res <- err
	}()
	require.Eventually(t, func() bool { return c.addCalls.Load() >= 1 }, 2*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-res, context.Canceled)

	_, err := f.MapPort(context.Background(), UDP, 41641, "retry")
	assert.ErrorContains(t, err, "still being cleaned up")

	close(release)
	require.Eventually(t, func() bool { return c.deleteCalls.Load() == 1 }, 2*time.Second, time.Millisecond)
	// The cleanup delete is still running: the port stays refused.
	time.Sleep(20 * time.Millisecond)
	_, err = f.MapPort(context.Background(), UDP, 41641, "retry")
	assert.ErrorContains(t, err, "still being cleaned up", "the port was released before the cleanup delete returned")

	close(deleteDone)
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.inflight) == 0
	}, 2*time.Second, time.Millisecond)
	_, err = f.MapPort(context.Background(), UDP, 41641, "retry")
	require.NoError(t, err)
	assert.Equal(t, int64(1), c.deleteCalls.Load(), "the new mapping was deleted by the old cleanup")
}

// A renewal that raced a teardown leaves alone a port the Forwarder has since
// mapped again: that mapping is live.
func TestForwarder_RacedRenewalSparesARemappedPort(t *testing.T) {
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)
	f.mu.Lock()
	old, teardowns := f.mappings[TCP], f.teardowns
	f.mu.Unlock()
	require.NoError(t, f.UnmapPort(context.Background()))
	_, err = f.MapPort(context.Background(), TCP, 30001, "again")
	require.NoError(t, err)

	assert.False(t, f.renew(old, c, teardowns))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), c.deleteCalls.Load(), "only UnmapPort's delete; the re-mapped port must survive")
}

// A renewal that timed out and was then torn down is removed when its add
// finally resolves: nothing else tracks it by then.
func TestForwarder_TimedOutRenewalThenTeardownIsRemoved(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{} // MapPort's add goes through; the renewal's waits
	c := &fakeIGD{addBlock: gate}
	f := newTestForwarder(t, c)
	f.renewWait = 20 * time.Millisecond
	_, err := f.MapPort(context.Background(), TCP, 30001, "test")
	require.NoError(t, err)
	f.mu.Lock()
	m, teardowns := f.mappings[TCP], f.teardowns
	f.mu.Unlock()

	assert.True(t, f.renew(m, c, teardowns), "the renewal timed out with no teardown yet")
	require.NoError(t, f.UnmapPort(context.Background()))
	require.Equal(t, int64(1), c.deleteCalls.Load())

	gate <- struct{}{} // the renewal's add lands after the teardown
	require.Eventually(t, func() bool { return c.deleteCalls.Load() == 2 }, 2*time.Second, time.Millisecond,
		"the renewal re-added the mapping after teardown and nothing removed it")
}

// Only a SOAP fault, the gateway's refusal, is a definite failure. Any other
// UPnP error may come after the gateway acted, so its outcome is unknown.
func TestUPnPOutcome(t *testing.T) {
	assert.NoError(t, upnpOutcome(nil))
	fault := &soap.SOAPFaultError{FaultCode: "s:Client", FaultString: "UPnPError"}
	assert.Same(t, fault, upnpOutcome(fault))
	assert.False(t, mayHaveLanded(upnpOutcome(fault)))
	err := upnpOutcome(errors.New("goupnp: error decoding response body: EOF"))
	assert.ErrorIs(t, err, errOutcomeUnknown)
	assert.True(t, mayHaveLanded(err))
}

// UPnP calls are bounded, so a gateway that accepts the connection and never
// answers can't hold an add, or its port, forever. An add that runs out of
// time is an unknown outcome, which gets the cleanup delete.
func TestUPnPCallsAreBounded(t *testing.T) {
	prev := upnpCallTimeout
	upnpCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upnpCallTimeout = prev })
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hang); srv.Close() })
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	w := wanIPv2Wrapper{c: &internetgateway2.WANIPConnection2{ServiceClient: goupnp.ServiceClient{
		SOAPClient: soap.NewSOAPClient(*u),
		Service:    &goupnp.Service{ServiceType: internetgateway2.URN_WANIPConnection_2},
	}}}

	start := time.Now()
	err = w.AddPortMapping("", 30001, TCP, 30001, "192.168.1.10", true, "test", 3600)
	assert.ErrorIs(t, err, errOutcomeUnknown)
	assert.Error(t, w.DeletePortMapping("", 30001, TCP))
	_, err = w.GetExternalIPAddress()
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "a UPnP call waited on a gateway that never answered")
}
