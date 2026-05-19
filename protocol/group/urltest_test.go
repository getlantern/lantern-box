package group

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/urltest"
	sbLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"

	"github.com/getlantern/lantern-box/constant"
	isync "github.com/getlantern/lantern-box/internal/sync"
)

// blockingCloseConn wraps a net.Conn whose Close blocks until released is closed.
// It simulates transports like WATER that call WaitWorker inside Close.
type blockingCloseConn struct {
	net.Conn
	released <-chan struct{}
}

func (c *blockingCloseConn) Close() error {
	<-c.released
	return c.Conn.Close()
}

type stubNetDialer struct {
	dialFn func(ctx context.Context, network string, dest metadata.Socksaddr) (net.Conn, error)
}

func (d *stubNetDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	return d.dialFn(ctx, network, destination)
}

func (d *stubNetDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("not supported")
}

func TestAsyncCloseConn_CloseReturnsImmediately(t *testing.T) {
	released := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-released:
		default:
			close(released)
		}
	})
	p1, p2 := net.Pipe()
	defer p2.Close()

	c := &asyncCloseConn{Conn: &blockingCloseConn{Conn: p1, released: released}}
	start := time.Now()
	_ = c.Close()
	assert.Less(t, time.Since(start), 100*time.Millisecond)
}

// TestURLTestGET_BlockingConnCloseDoesNotBlockReturn is a regression test for
// the case where the HTTP transport's context-cancellation path calls
// conn.Close() synchronously. Before asyncCloseConn was applied to the conn
// passed to the transport's DialContext, WATER's WaitWorker would block
// client.Do() for 35–90 s after the test context expired.
//
// The blocking window is simulated by keeping released closed for 3 s after
// the request context expires. Without asyncCloseConn, client.Do blocks for
// those 3 s, making elapsed > 2 s and the assertion fail. With the fix,
// Close() dispatches to a goroutine, client.Do returns at ~200 ms, and
// elapsed stays well under 2 s.
func TestURLTestGET_BlockingConnCloseDoesNotBlockReturn(t *testing.T) {
	released := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }

	// The server delays its response well beyond the context timeout so the
	// HTTP transport is forced to cancel and close the conn.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	dialer := &stubNetDialer{
		dialFn: func(ctx context.Context, network string, dest metadata.Socksaddr) (net.Conn, error) {
			c, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 2*time.Second)
			if err != nil {
				return nil, err
			}
			return &blockingCloseConn{Conn: c, released: released}, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Release the blocking conn 3 s after the request context expires.
	// Without asyncCloseConn, the HTTP transport calls blockingCloseConn.Close()
	// synchronously inside client.Do, so urlTestGET can't return until released fires.
	// With the fix, Close() is dispatched to a goroutine and urlTestGET returns at ~200 ms.
	time.AfterFunc(200*time.Millisecond+3*time.Second, release)

	start := time.Now()
	_, _ = urlTestGET(ctx, "http://"+srv.Listener.Addr().String()+"/", dialer)
	elapsed := time.Since(start)

	// Unblock the async-close goroutine before closing the server so the server
	// handler can exit and srv.Close() returns promptly.
	release()
	srv.Close()

	assert.Less(t, elapsed, 2*time.Second,
		"urlTestGET blocked for %v after context expiry; asyncCloseConn should prevent blocking", elapsed)
}

// TestURLTestGET_BlockingDialContextDoesNotBlockReturn is a regression test for
// outbounds whose DialContext ignores context cancellation because they block
// in host-function callbacks that Go's runtime cannot interrupt (e.g. WATER's
// WASM TCP dial). Before the goroutine-race fix, urlTestGET blocked for the
// full OS TCP timeout (~87 s) even when the testCtx had already expired.
//
// The blocking window is simulated by a dialer that ignores ctx and returns
// only after released is closed, 3 s after the request context expires.
// Without the fix, urlTestGET blocks for those 3 s. With the fix, the
// ctx.Done() select arm fires and urlTestGET returns at ~200 ms.
func TestURLTestGET_BlockingDialContextDoesNotBlockReturn(t *testing.T) {
	released := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }

	dialer := &stubNetDialer{
		dialFn: func(ctx context.Context, network string, dest metadata.Socksaddr) (net.Conn, error) {
			select {
			case <-released:
			case <-time.After(10 * time.Second):
			}
			return nil, errors.New("never connected")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Release 3 s after the context expires; without the fix urlTestGET blocks for those 3 s.
	time.AfterFunc(200*time.Millisecond+3*time.Second, release)

	start := time.Now()
	_, _ = urlTestGET(ctx, "http://192.0.2.1/", dialer)
	elapsed := time.Since(start)

	release()

	assert.Less(t, elapsed, 2*time.Second,
		"urlTestGET blocked for %v after context expiry; DialContext goroutine race should prevent blocking", elapsed)
}

func TestUpdateSelected(t *testing.T) {
	outbounds := map[string]uint16{
		"kangaskhan": 36,
		"koffing":    52,
		"lickitung":  55,
		"growlithe":  56,
		"machamp":    138,
		"golduck":    142,
		"hitmonlee":  204,
	}
	testGroup := &urlTestGroup{
		tags:                []string{},
		outbounds:           isync.TypedMap[string, adapter.Outbound]{},
		tolerance:           50,
		history:             urltest.NewHistoryStorage(),
		selectedOutboundTCP: common.TypedValue[adapter.Outbound]{},
		selectedOutboundUDP: common.TypedValue[adapter.Outbound]{},
	}
	for tag, delay := range outbounds {
		testGroup.tags = append(testGroup.tags, tag)
		testGroup.outbounds.Store(tag, &mockOutbound{tag: tag})
		testGroup.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  time.Now(),
			Delay: delay,
		})
	}

	o, _ := testGroup.outbounds.Load("golduck")
	testGroup.selectedOutboundTCP.Store(o)
	testGroup.selectedOutboundUDP.Store(o)

	testGroup.updateSelected()

	want := []string{"kangaskhan", "koffing", "lickitung", "growlithe"}
	assert.Contains(t, want, testGroup.selectedOutboundTCP.Load().Tag())
	assert.Contains(t, want, testGroup.selectedOutboundUDP.Load().Tag())
}

func TestMarkFailedAndReselect_SwitchesToHealthyOutbound(t *testing.T) {
	g := &urlTestGroup{
		tags:      []string{},
		outbounds: isync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	// Wide delay gaps so pickBestOutbound is deterministic regardless of map
	// iteration order: beta beats gamma by more than tolerance either way.
	delays := map[string]uint16{"alpha": 50, "beta": 1000, "gamma": 2000}
	outboundByTag := make(map[string]adapter.Outbound, len(delays))
	for tag, delay := range delays {
		ob := &mockOutbound{tag: tag}
		g.tags = append(g.tags, tag)
		g.outbounds.Store(tag, ob)
		g.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  time.Now(),
			Delay: delay,
		})
		outboundByTag[tag] = ob
	}
	g.selectedOutboundTCP.Store(outboundByTag["alpha"])
	g.selectedOutboundUDP.Store(outboundByTag["alpha"])

	g.markFailedAndReselect(outboundByTag["alpha"])

	require := assert.New(t)
	require.Nil(g.history.LoadURLTestHistory("alpha"))
	require.Equal("beta", g.selectedOutboundTCP.Load().Tag(),
		"expected reselection to next-best outbound after failure")
	require.Equal("beta", g.selectedOutboundUDP.Load().Tag())
}

// retryMode parameterizes the table-driven tests across DialContext (tcp) and
// ListenPacket (udp), which share the same retry machinery.
type retryMode struct {
	method      string
	setSelected func(g *urlTestGroup, o adapter.Outbound)
	invoke      func(s *MutableURLTest, ctx context.Context) (any, error)
}

var retryModes = []retryMode{
	{
		method:      "DialContext",
		setSelected: func(g *urlTestGroup, o adapter.Outbound) { g.selectedOutboundTCP.Store(o) },
		invoke: func(s *MutableURLTest, ctx context.Context) (any, error) {
			return s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		},
	},
	{
		method:      "ListenPacket",
		setSelected: func(g *urlTestGroup, o adapter.Outbound) { g.selectedOutboundUDP.Store(o) },
		invoke: func(s *MutableURLTest, ctx context.Context) (any, error) {
			return s.ListenPacket(ctx, metadata.Socksaddr{})
		},
	},
}

func newRetryRig(mode retryMode, ctx context.Context, delays map[string]uint16, firstSelected string) (*MutableURLTest, map[string]*mockOutbound) {
	g := &urlTestGroup{
		ctx:       ctx,
		logger:    sbLog.NewNOPFactory().Logger(),
		outbounds: isync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	outbounds := make(map[string]*mockOutbound, len(delays))
	for tag, delay := range delays {
		ob := &mockOutbound{tag: tag}
		g.tags = append(g.tags, tag)
		g.outbounds.Store(tag, ob)
		g.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{Time: time.Now(), Delay: delay})
		outbounds[tag] = ob
	}
	if first, ok := outbounds[firstSelected]; ok {
		mode.setSelected(g, first)
	}
	s := &MutableURLTest{
		Adapter: O.NewAdapter(constant.TypeMutableURLTest, "test", []string{"tcp", "udp"}, nil),
		ctx:     ctx,
		logger:  sbLog.NewNOPFactory().Logger(),
		group:   g,
	}
	return s, outbounds
}

func TestMutableURLTest_RetriesEachOutbound(t *testing.T) {
	for _, mode := range retryModes {
		t.Run(mode.method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			delays := map[string]uint16{"alpha": 50, "beta": 1000, "gamma": 2000}
			s, outbounds := newRetryRig(mode, ctx, delays, "alpha")

			outbounds["alpha"].On(mode.method).Return(nil, assert.AnError)
			outbounds["beta"].On(mode.method).Return(nil, assert.AnError)
			outbounds["gamma"].On(mode.method).Return(&net.IPConn{}, nil)

			conn, err := mode.invoke(s, ctx)
			assert.NoError(t, err)
			assert.NotNil(t, conn)
			for tag, ob := range outbounds {
				ob.AssertExpectations(t)
				assert.Equalf(t, 1, callCount(ob, mode.method),
					"outbound %s should have been attempted exactly once", tag)
			}
		})
	}
}

func TestMutableURLTest_AllFail(t *testing.T) {
	for _, mode := range retryModes {
		t.Run(mode.method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			delays := map[string]uint16{"alpha": 50, "beta": 1000}
			s, outbounds := newRetryRig(mode, ctx, delays, "alpha")
			for _, ob := range outbounds {
				ob.On(mode.method).Return(nil, assert.AnError)
			}

			conn, err := mode.invoke(s, ctx)
			assert.Error(t, err)
			assert.Nil(t, conn)
			assert.True(t, errors.Is(err, ErrAllOutboundsFailed),
				"err should be ErrAllOutboundsFailed so callers can trigger a URL test; got: %v", err)
			assert.True(t, errors.Is(err, assert.AnError),
				"the last underlying error should still be wrapped; got: %v", err)
			for tag, ob := range outbounds {
				assert.Equalf(t, 1, callCount(ob, mode.method),
					"outbound %s should have been attempted exactly once", tag)
			}
		})
	}
}

func TestMutableURLTest_RetryCap(t *testing.T) {
	for _, mode := range retryModes {
		t.Run(mode.method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// More outbounds than maxOutboundAttempts so the cap is the binding constraint.
			delays := map[string]uint16{}
			for i, tag := range []string{"a", "b", "c", "d", "e"} {
				delays[tag] = uint16(100 * (i + 1))
			}
			s, outbounds := newRetryRig(mode, ctx, delays, "a")
			for _, ob := range outbounds {
				ob.On(mode.method).Return(nil, assert.AnError)
			}

			conn, err := mode.invoke(s, ctx)
			assert.Error(t, err)
			assert.Nil(t, conn)
			assert.True(t, errors.Is(err, ErrAllOutboundsFailed),
				"err should be ErrAllOutboundsFailed; got: %v", err)

			total := 0
			for _, ob := range outbounds {
				total += callCount(ob, mode.method)
			}
			assert.Equalf(t, maxOutboundAttempts, total,
				"attempts should equal maxOutboundAttempts; got %d", total)
		})
	}
}

func callCount(m *mockOutbound, method string) int {
	n := 0
	for _, c := range m.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func TestPickBestOutbound_FallbackSkipsCurrent(t *testing.T) {
	g := &urlTestGroup{
		tags:      []string{"alpha", "beta"},
		outbounds: isync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	alpha := &mockOutbound{tag: "alpha"}
	beta := &mockOutbound{tag: "beta"}
	g.outbounds.Store("alpha", alpha)
	g.outbounds.Store("beta", beta)

	picked := g.pickBestOutbound("tcp", alpha, nil)
	assert.Equal(t, "beta", picked.Tag(),
		"fallback path should skip current and return the other outbound")
}
