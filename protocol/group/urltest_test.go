package group

import (
	"context"
	"errors"
	"net"
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
	"github.com/getlantern/lantern-box/internal/sync"
)

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
		outbounds:           sync.TypedMap[string, adapter.Outbound]{},
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
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
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
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
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
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
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
