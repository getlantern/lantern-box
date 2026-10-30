package group

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sboxLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

// syncOutboundManager is an OutboundManager safe for the concurrent lookups
// the probe goroutines make while a test swaps outbounds under a tag.
type syncOutboundManager struct {
	adapter.OutboundManager
	mu        sync.Mutex
	outbounds map[string]adapter.Outbound
}

func newSyncOutboundManager(outbounds ...adapter.Outbound) *syncOutboundManager {
	m := &syncOutboundManager{outbounds: make(map[string]adapter.Outbound)}
	for _, o := range outbounds {
		m.outbounds[o.Tag()] = o
	}
	return m
}

func (m *syncOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outbounds[tag]
	return o, ok
}

func (m *syncOutboundManager) set(o adapter.Outbound) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outbounds[o.Tag()] = o
}

// trackedConn records whether it was closed, so a test can tell an
// interrupted connection from an untouched one.
type trackedConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// dialStats counts an outbound's dials and keeps the last conn it handed out.
type dialStats struct {
	atomic.Int32
	last atomic.Pointer[trackedConn]
}

// countingOutbound returns a mockOutbound whose dials hand out trackedConns
// and are counted.
func countingOutbound(tag string) (*mockOutbound, *dialStats) {
	stats := &dialStats{}
	ob := &mockOutbound{tag: tag}
	ob.dial = func(context.Context) (net.Conn, error) {
		stats.Add(1)
		c, peer := net.Pipe()
		go func() { _, _ = peer.Read(make([]byte, 1)) }()
		tc := &trackedConn{Conn: c}
		stats.last.Store(tc)
		return tc, nil
	}
	return ob, stats
}

// probeServer returns a test URL and an outbound dialer that reaches it, so a
// URL test through the outbound succeeds.
func probeServer(t *testing.T) (string, func(context.Context) (net.Conn, error)) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return srv.URL, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func newTestSelector(t *testing.T, mgr adapter.OutboundManager, tags ...string) *MutableSelector {
	ctx := service.ContextWith[adapter.OutboundManager](context.Background(), mgr)
	out, err := NewMutableSelector(ctx, nil, sboxLog.NewNOPFactory().Logger(), "selector",
		option.MutableSelectorOutboundOptions{Outbounds: tags})
	require.NoError(t, err)
	s := out.(*MutableSelector)
	require.NoError(t, s.Start())
	return s
}

func newTestURLTest(t *testing.T, ctx context.Context, mgr adapter.OutboundManager, link string, tags ...string) *MutableURLTest {
	ctx = service.ContextWith[adapter.OutboundManager](ctx, mgr)
	out, err := NewMutableURLTest(ctx, nil, sboxLog.NewNOPFactory().Logger(), "urltest",
		option.MutableURLTestOutboundOptions{Outbounds: tags, URL: link})
	require.NoError(t, err)
	u := out.(*MutableURLTest)
	require.NoError(t, u.Start())
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func TestURLTestGroup_ProbeOnEmptyGroupDoesNotLatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, dial := probeServer(t)
	ob := &mockOutbound{tag: "a", dial: dial}
	mgr := newSyncOutboundManager(ob)
	u := newTestURLTest(t, ctx, mgr, link)
	g := u.group

	// A probe while the group is empty must not leave checking latched.
	_, err := g.URLTest(ctx)
	require.NoError(t, err)
	assert.False(t, g.checking.Load(), "checking must be reset after a probe on an empty group")

	n, err := g.Add([]string{"a"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	result, err := g.URLTest(ctx)
	require.NoError(t, err)
	assert.Contains(t, result, "a", "the group must probe again once it has a member")
	assert.NotNil(t, g.history.LoadURLTestHistory("a"))
}

func TestMutableSelector_ReplaceKeepsSelection(t *testing.T) {
	oldA, _ := countingOutbound("a")
	b, _ := countingOutbound("b")
	mgr := newSyncOutboundManager(oldA, b)
	s := newTestSelector(t, mgr, "a", "b")
	require.Equal(t, "a", s.Now())

	stop := make(chan struct{})
	var wrong atomic.Value
	var polls sync.WaitGroup
	polls.Add(1)
	go func() {
		defer polls.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if now := s.Now(); now != "a" {
				wrong.Store(now)
			}
		}
	}()
	for range 200 {
		next, _ := countingOutbound("a")
		mgr.set(next)
		n, err := s.Replace("a")
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	close(stop)
	polls.Wait()
	assert.Nil(t, wrong.Load(), "selector reported another tag during Replace")
	assert.Equal(t, "a", s.Now())
	assert.ElementsMatch(t, []string{"a", "b"}, s.All())
}

func TestMutableSelector_ReplaceRoutesToNewObject(t *testing.T) {
	oldA, oldDials := countingOutbound("a")
	b, bDials := countingOutbound("b")
	mgr := newSyncOutboundManager(oldA, b)
	s := newTestSelector(t, mgr, "a", "b")

	newA, newDials := countingOutbound("a")
	mgr.set(newA)
	n, err := s.Replace("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Equal(t, int32(0), oldDials.Load(), "dial went to the replaced object")
	assert.Equal(t, int32(1), newDials.Load())
	assert.Equal(t, int32(0), bDials.Load())

	// Replacing an unselected member swaps its object without selecting it.
	newB, newBDials := countingOutbound("b")
	mgr.set(newB)
	n, err = s.Replace("b")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, "a", s.Now())
	require.True(t, s.SelectOutbound("b"))
	conn2, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer func() { _ = conn2.Close() }()
	assert.Equal(t, int32(0), bDials.Load())
	assert.Equal(t, int32(1), newBDials.Load())
}

func TestMutableSelector_ReplaceDoesNotInterruptConnections(t *testing.T) {
	a, _ := countingOutbound("a")
	b, bDials := countingOutbound("b")
	mgr := newSyncOutboundManager(a, b)
	s := newTestSelector(t, mgr, "a", "b")

	// A selection change interrupts every connection in the group, so the
	// only live connections are through the selected tag.
	require.True(t, s.SelectOutbound("b"))
	connB, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer func() { _ = connB.Close() }()
	trackedB := bDials.last.Load()

	newA, _ := countingOutbound("a")
	mgr.set(newA)
	_, err = s.Replace("a")
	require.NoError(t, err)
	assert.False(t, trackedB.closed.Load(), "replacing another tag interrupted a connection")

	newB, _ := countingOutbound("b")
	mgr.set(newB)
	_, err = s.Replace("b")
	require.NoError(t, err)
	assert.False(t, trackedB.closed.Load(), "replacing the selected tag interrupted its existing connection")
	assert.Equal(t, "b", s.Now())

	// A selection change does interrupt, so the assertions above would catch one.
	require.True(t, s.SelectOutbound("a"))
	assert.True(t, trackedB.closed.Load())
}

func TestMutableSelector_ReplaceIgnoresMissingTags(t *testing.T) {
	a, _ := countingOutbound("a")
	c, _ := countingOutbound("c")
	mgr := newSyncOutboundManager(a, c)
	s := newTestSelector(t, mgr, "a")

	n, err := s.Replace("c", "nope")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, []string{"a"}, s.All())
	assert.Equal(t, "a", s.Now())
}

func TestMutableSelector_ReplaceMemberGoneFromManager(t *testing.T) {
	a, aDials := countingOutbound("a")
	mgr := newSyncOutboundManager(a)
	s := newTestSelector(t, mgr, "a")

	mgr.mu.Lock()
	delete(mgr.outbounds, "a")
	mgr.mu.Unlock()
	n, err := s.Replace("a")
	assert.Error(t, err)
	assert.Equal(t, 0, n)
	// The member keeps its old object rather than leaving the group.
	assert.Equal(t, "a", s.Now())
	conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Equal(t, int32(1), aDials.Load())
}

func TestMutableURLTest_ReplaceSoleMemberNeverEmpties(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := countingOutbound("a")
	mgr := newSyncOutboundManager(a)
	u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")
	require.Equal(t, "a", u.Now())

	stop := make(chan struct{})
	var wrong atomic.Value
	var polls sync.WaitGroup
	polls.Add(1)
	go func() {
		defer polls.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if now := u.Now(); now != "a" {
				wrong.Store(now)
			}
		}
	}()
	for range 200 {
		next, _ := countingOutbound("a")
		mgr.set(next)
		n, err := u.Replace("a")
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	close(stop)
	polls.Wait()
	assert.Nil(t, wrong.Load(), "url-test group reported another selection during Replace")

	u.group.access.Lock()
	tags := append([]string(nil), u.group.tags...)
	u.group.access.Unlock()
	assert.Equal(t, []string{"a"}, tags)
}

func TestMutableURLTest_ReplaceRoutesToNewObjectAndResetsHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldA, oldDials := countingOutbound("a")
	b, bDials := countingOutbound("b")
	mgr := newSyncOutboundManager(oldA, b)
	u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a", "b")
	u.group.history.StoreURLTestHistory("a", &adapter.URLTestHistory{Time: time.Now(), Delay: 10})
	u.group.history.StoreURLTestHistory("b", &adapter.URLTestHistory{Time: time.Now(), Delay: 500})
	u.group.updateSelected()
	require.Equal(t, "a", u.Now())

	newA, newDials := countingOutbound("a")
	mgr.set(newA)
	n, err := u.Replace("a", "missing")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Nil(t, u.group.history.LoadURLTestHistory("a"), "a replaced member's URL-test history must be reset")
	assert.NotNil(t, u.group.history.LoadURLTestHistory("b"), "other members keep their history")
	assert.Equal(t, "a", u.Now(), "the replaced member stays selected until a probe re-evaluates")

	conn, err := u.DialContext(ctx, "tcp", metadata.Socksaddr{})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Equal(t, int32(0), oldDials.Load(), "dial went to the replaced object")
	assert.Equal(t, int32(1), newDials.Load())
	assert.Equal(t, int32(0), bDials.Load())
}

func TestMutableURLTest_ReplaceTriggersProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, dial := probeServer(t)
	var oldProbes atomic.Int32
	oldA := &mockOutbound{tag: "a", dial: func(ctx context.Context) (net.Conn, error) {
		oldProbes.Add(1)
		return dial(ctx)
	}}
	mgr := newSyncOutboundManager(oldA)
	u := newTestURLTest(t, ctx, mgr, link, "a")
	g := u.group
	g.access.Lock()
	g.started = true
	g.pauseMgr = &mockPauseManager{}
	g.access.Unlock()
	g.history.StoreURLTestHistory("a", &adapter.URLTestHistory{Time: time.Now(), Delay: 10})

	var newProbes atomic.Int32
	newA := &mockOutbound{tag: "a", dial: func(ctx context.Context) (net.Conn, error) {
		newProbes.Add(1)
		return dial(ctx)
	}}
	mgr.set(newA)
	n, err := u.Replace("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Eventually(t, func() bool {
		return g.history.LoadURLTestHistory("a") != nil && newProbes.Load() > 0
	}, 5*time.Second, 10*time.Millisecond, "Replace must trigger a probe of the new object")
	assert.Equal(t, int32(0), oldProbes.Load())
	assert.Eventually(t, func() bool { return !g.checking.Load() }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "a", u.Now())
}

func TestMutableURLTest_ReplaceIgnoresMissingTagsAndClosedGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := countingOutbound("a")
	c, _ := countingOutbound("c")
	mgr := newSyncOutboundManager(a, c)
	u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")

	n, err := u.Replace("c", "nope")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, []string{"a"}, u.All())

	cancel()
	require.NoError(t, u.Close())
	_, err = u.Replace("a")
	assert.ErrorIs(t, err, lbA.ErrGroupClosed)
}

// blockingHistory wraps a history storage so that, once armed, the next load
// of a tag reads the stored value and then blocks until released, holding a
// selection computation mid-pick.
type blockingHistory struct {
	adapter.URLTestHistoryStorage
	armedTag atomic.Value
	blocked  chan struct{}
	release  chan struct{}
}

func (h *blockingHistory) LoadURLTestHistory(tag string) *adapter.URLTestHistory {
	v := h.URLTestHistoryStorage.LoadURLTestHistory(tag)
	if armed, _ := h.armedTag.Load().(string); armed == tag && h.armedTag.CompareAndSwap(tag, "") {
		close(h.blocked)
		<-h.release
	}
	return v
}

func TestURLTestGroup_ReplaceDuringSelectionUpdatePublishesCurrentMember(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldA, _ := countingOutbound("a")
	b, _ := countingOutbound("b")
	mgr := newSyncOutboundManager(oldA, b)
	u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a", "b")
	g := u.group
	history := &blockingHistory{
		URLTestHistoryStorage: g.history,
		blocked:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	g.history = history
	history.StoreURLTestHistory("a", &adapter.URLTestHistory{Time: time.Now(), Delay: 10})
	history.StoreURLTestHistory("b", &adapter.URLTestHistory{Time: time.Now(), Delay: 500})
	g.selectedOutboundTCP.Store(b)
	g.selectedOutboundUDP.Store(b)

	// Hold a selection update after it has seen the old "a" as the best member.
	history.armedTag.Store("a")
	updated := make(chan struct{})
	go func() {
		defer close(updated)
		g.updateSelected()
	}()
	<-history.blocked

	newA, _ := countingOutbound("a")
	mgr.set(newA)
	replaced := make(chan struct{})
	go func() {
		defer close(replaced)
		_, _ = u.Replace("a")
	}()
	select {
	case <-replaced:
	case <-time.After(100 * time.Millisecond):
	}
	close(history.release)
	<-updated
	<-replaced

	assert.Same(t, newA, g.selectedOutboundTCP.Load(), "selection must not publish the replaced outbound")
	assert.Same(t, newA, g.selectedOutboundUDP.Load(), "selection must not publish the replaced outbound")
}

// startedURLTest returns a started group over the given outbounds, as PostStart
// leaves it but without its initial probe.
func startedURLTest(t *testing.T, ctx context.Context, mgr adapter.OutboundManager, link string, tags ...string) *MutableURLTest {
	u := newTestURLTest(t, ctx, mgr, link, tags...)
	u.group.access.Lock()
	u.group.started = true
	u.group.pauseMgr = &mockPauseManager{}
	u.group.access.Unlock()
	return u
}

func TestURLTestGroup_ReplaceDuringRegularTestProbesReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, dial := probeServer(t)
	oldEntered := make(chan struct{})
	oldRelease := make(chan struct{})
	// The old instance fails once released: were its result kept, it would
	// clear the replacement's history.
	oldA := &mockOutbound{tag: "a", dial: func(context.Context) (net.Conn, error) {
		close(oldEntered)
		<-oldRelease
		return nil, assert.AnError
	}}
	mgr := newSyncOutboundManager(oldA)
	u := startedURLTest(t, ctx, mgr, link, "a")
	g := u.group

	regular := make(chan struct{})
	go func() {
		defer close(regular)
		g.CheckOutbounds(true)
	}()
	<-oldEntered

	var newProbes atomic.Int32
	newA := &mockOutbound{tag: "a", dial: func(ctx context.Context) (net.Conn, error) {
		newProbes.Add(1)
		return dial(ctx)
	}}
	mgr.set(newA)
	n, err := u.Replace("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Eventually(t, func() bool {
		return newProbes.Load() > 0 && g.history.LoadURLTestHistory("a") != nil
	}, 5*time.Second, 10*time.Millisecond, "the replacement must be probed while the regular test runs")

	close(oldRelease)
	<-regular
	assert.NotNil(t, g.history.LoadURLTestHistory("a"), "the superseded instance's result must be dropped")
	assert.Same(t, newA, g.selectedOutboundTCP.Load())
	assert.Equal(t, "a", u.Now())
}

func TestURLTestGroup_URLTestHonorsDeadlineDuringReplaces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalling := func() *mockOutbound {
		return &mockOutbound{tag: "a", dial: func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	}
	mgr := newSyncOutboundManager(stalling())
	u := startedURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")

	stop := make(chan struct{})
	var replacing sync.WaitGroup
	replacing.Add(1)
	go func() {
		defer replacing.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			mgr.set(stalling())
			_, _ = u.Replace("a")
		}
	}()
	defer func() {
		close(stop)
		replacing.Wait()
	}()

	testCtx, testCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer testCancel()
	start := time.Now()
	_, err := u.URLTest(testCtx)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second, "URLTest must return within its caller's deadline")
}

func TestURLTestGroup_ReplacedAgainDropsInFlightProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, dial := probeServer(t)
	a0, _ := countingOutbound("a")
	mgr := newSyncOutboundManager(a0)
	u := startedURLTest(t, ctx, mgr, link, "a")
	g := u.group

	a1Entered := make(chan struct{})
	a1Release := make(chan struct{})
	var a1Returned atomic.Bool
	a1 := &mockOutbound{tag: "a", dial: func(context.Context) (net.Conn, error) {
		close(a1Entered)
		<-a1Release
		a1Returned.Store(true)
		return nil, assert.AnError
	}}
	mgr.set(a1)
	_, err := u.Replace("a")
	require.NoError(t, err)
	<-a1Entered

	a2 := &mockOutbound{tag: "a", dial: dial}
	mgr.set(a2)
	_, err = u.Replace("a")
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return g.history.LoadURLTestHistory("a") != nil
	}, 5*time.Second, 10*time.Millisecond, "the latest replacement must be probed")

	close(a1Release)
	require.Eventually(t, a1Returned.Load, time.Second, time.Millisecond)
	// Give a1's probe time to settle its failed result.
	time.Sleep(50 * time.Millisecond)
	assert.NotNil(t, g.history.LoadURLTestHistory("a"), "the superseded probe's failure must be dropped")
	assert.Same(t, a2, g.selectedOutboundTCP.Load())
}

func blockingProbe(tag string) (ob *mockOutbound, entered, release chan struct{}) {
	entered = make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	ob = &mockOutbound{tag: tag, dial: func(ctx context.Context) (net.Conn, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, assert.AnError
	}}
	return ob, entered, release
}

func TestURLTestGroup_StaleResultKeepsPendingReplacementSelected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldA, oldEntered, oldRelease := blockingProbe("a")
	mgr := newSyncOutboundManager(oldA)
	u := startedURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")
	g := u.group

	regular := make(chan struct{})
	go func() {
		defer close(regular)
		g.CheckOutbounds(true)
	}()
	<-oldEntered

	newA, newEntered, newRelease := blockingProbe("a")
	defer close(newRelease)
	mgr.set(newA)
	_, err := u.Replace("a")
	require.NoError(t, err)
	<-newEntered

	// The regular test's result for the old instance lands before the
	// replacement's probe.
	close(oldRelease)
	<-regular
	assert.Same(t, newA, g.selectedOutboundTCP.Load())
	assert.Same(t, newA, g.selectedOutboundUDP.Load())
	assert.Equal(t, "a", u.Now())
}

func TestURLTestGroup_SupersededReplacementProbeKeepsSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a0, _ := countingOutbound("a")
	mgr := newSyncOutboundManager(a0)
	u := startedURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")
	g := u.group

	a1, a1Entered, a1Release := blockingProbe("a")
	mgr.set(a1)
	_, err := u.Replace("a")
	require.NoError(t, err)
	<-a1Entered

	a2, a2Entered, a2Release := blockingProbe("a")
	defer close(a2Release)
	mgr.set(a2)
	_, err = u.Replace("a")
	require.NoError(t, err)
	<-a2Entered

	close(a1Release)
	time.Sleep(50 * time.Millisecond)
	assert.Same(t, a2, g.selectedOutboundTCP.Load())
	assert.Equal(t, "a", u.Now())
}

func TestURLTestGroup_ReplaceWithNarrowerNetworks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Run("other member takes the network", func(t *testing.T) {
		a, _ := countingOutbound("a")
		b, _ := countingOutbound("b")
		mgr := newSyncOutboundManager(a, b)
		u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a", "b")
		g := u.group
		g.history.StoreURLTestHistory("a", &adapter.URLTestHistory{Time: time.Now(), Delay: 10})
		g.history.StoreURLTestHistory("b", &adapter.URLTestHistory{Time: time.Now(), Delay: 500})
		g.updateSelected()
		require.Same(t, a, g.selectedOutboundUDP.Load())

		tcpOnly, _ := countingOutbound("a")
		tcpOnly.networks = []string{"tcp"}
		mgr.set(tcpOnly)
		_, err := u.Replace("a")
		require.NoError(t, err)
		assert.Same(t, tcpOnly, g.selectedOutboundTCP.Load())
		assert.Same(t, b, g.selectedOutboundUDP.Load())
	})

	t.Run("sole member clears the network", func(t *testing.T) {
		a, _ := countingOutbound("a")
		mgr := newSyncOutboundManager(a)
		u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a")
		g := u.group
		require.Same(t, a, g.selectedOutboundUDP.Load())

		tcpOnly, _ := countingOutbound("a")
		tcpOnly.networks = []string{"tcp"}
		mgr.set(tcpOnly)
		_, err := u.Replace("a")
		require.NoError(t, err)
		assert.Same(t, tcpOnly, g.selectedOutboundTCP.Load())
		assert.Nil(t, g.selectedOutboundUDP.Load())
	})

	t.Run("selection seeding skips an unsupported current", func(t *testing.T) {
		tcpOnly, _ := countingOutbound("a")
		tcpOnly.networks = []string{"tcp"}
		b, _ := countingOutbound("b")
		mgr := newSyncOutboundManager(tcpOnly, b)
		u := newTestURLTest(t, ctx, mgr, "http://127.0.0.1:1", "a", "b")
		g := u.group
		g.history.StoreURLTestHistory("a", &adapter.URLTestHistory{Time: time.Now(), Delay: 10})
		g.history.StoreURLTestHistory("b", &adapter.URLTestHistory{Time: time.Now(), Delay: 500})
		assert.Same(t, b, g.pickBestOutbound("udp", tcpOnly, nil))
	})
}

// blockingStore blocks the first history write for a tag once armed, holding
// a probe between its still-current check and its write.
type blockingStore struct {
	adapter.URLTestHistoryStorage
	armedTag atomic.Value
	blocked  chan struct{}
	release  chan struct{}
}

func (h *blockingStore) StoreURLTestHistory(tag string, history *adapter.URLTestHistory) {
	if armed, _ := h.armedTag.Load().(string); armed == tag && h.armedTag.CompareAndSwap(tag, "") {
		close(h.blocked)
		<-h.release
	}
	h.URLTestHistoryStorage.StoreURLTestHistory(tag, history)
}

func TestURLTestGroup_RemoveDuringReplacementProbeWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, dial := probeServer(t)
	oldA, _ := countingOutbound("a")
	b, _ := countingOutbound("b")
	mgr := newSyncOutboundManager(oldA, b)
	u := startedURLTest(t, ctx, mgr, link, "a", "b")
	g := u.group
	history := &blockingStore{
		URLTestHistoryStorage: g.history,
		blocked:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	g.history = history
	history.StoreURLTestHistory("b", &adapter.URLTestHistory{Time: time.Now(), Delay: 500})
	history.armedTag.Store("a")

	newA := &mockOutbound{tag: "a", dial: dial}
	mgr.set(newA)
	_, err := u.Replace("a")
	require.NoError(t, err)
	<-history.blocked

	removed := make(chan struct{})
	go func() {
		defer close(removed)
		_, _ = u.Remove("a")
	}()
	select {
	case <-removed:
	case <-time.After(100 * time.Millisecond):
	}
	close(history.release)
	<-removed

	assert.Nil(t, g.history.LoadURLTestHistory("a"), "a removed member's history must not be restored")
	g.updateSelected()
	assert.Same(t, b, g.selectedOutboundTCP.Load())
	assert.Equal(t, []string{"b"}, u.All())
}
