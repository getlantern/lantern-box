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

func (m *mockOutbound) Tag() string {
	return m.tag
}

func (m *mockOutbound) Network() []string {
	return []string{"tcp", "udp"}
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

func TestMutableURLTest_DialContext_RetriesEachOutbound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delays := map[string]uint16{"alpha": 50, "beta": 1000, "gamma": 2000}
	outboundByTag := make(map[string]*mockOutbound, len(delays))
	g := &urlTestGroup{
		ctx:       ctx,
		logger:    sbLog.NewNOPFactory().Logger(),
		tags:      []string{},
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	for tag, delay := range delays {
		ob := &mockOutbound{tag: tag}
		g.tags = append(g.tags, tag)
		g.outbounds.Store(tag, ob)
		g.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{Time: time.Now(), Delay: delay})
		outboundByTag[tag] = ob
	}
	g.selectedOutboundTCP.Store(outboundByTag["alpha"])

	s := &MutableURLTest{
		Adapter: O.NewAdapter(constant.TypeMutableURLTest, "test", []string{"tcp", "udp"}, nil),
		ctx:     ctx,
		logger:  sbLog.NewNOPFactory().Logger(),
		group:   g,
	}

	outboundByTag["alpha"].On("DialContext").Return(nil, assert.AnError)
	outboundByTag["beta"].On("DialContext").Return(nil, assert.AnError)
	outboundByTag["gamma"].On("DialContext").Return(&net.IPConn{}, nil)

	conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
	assert.NoError(t, err)
	assert.NotNil(t, conn)
	for tag, ob := range outboundByTag {
		ob.AssertExpectations(t)
		assert.Equalf(t, 1, callCount(ob, "DialContext"), "outbound %s should have been dialed exactly once", tag)
	}
}

func TestMutableURLTest_DialContext_AllFail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delays := map[string]uint16{"alpha": 50, "beta": 1000}
	outboundByTag := make(map[string]*mockOutbound, len(delays))
	g := &urlTestGroup{
		ctx:       ctx,
		logger:    sbLog.NewNOPFactory().Logger(),
		tags:      []string{},
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	for tag, delay := range delays {
		ob := &mockOutbound{tag: tag}
		g.tags = append(g.tags, tag)
		g.outbounds.Store(tag, ob)
		g.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{Time: time.Now(), Delay: delay})
		outboundByTag[tag] = ob
	}
	g.selectedOutboundTCP.Store(outboundByTag["alpha"])

	s := &MutableURLTest{
		Adapter: O.NewAdapter(constant.TypeMutableURLTest, "test", []string{"tcp", "udp"}, nil),
		ctx:     ctx,
		logger:  sbLog.NewNOPFactory().Logger(),
		group:   g,
	}

	for _, ob := range outboundByTag {
		ob.On("DialContext").Return(nil, assert.AnError)
	}

	conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
	assert.Error(t, err)
	assert.Nil(t, conn)
	assert.True(t, errors.Is(err, ErrAllOutboundsFailed),
		"err should be ErrAllOutboundsFailed so callers can trigger a URL test; got: %v", err)
	assert.True(t, errors.Is(err, assert.AnError),
		"the last underlying dial error should still be wrapped; got: %v", err)
	for tag, ob := range outboundByTag {
		assert.Equalf(t, 1, callCount(ob, "DialContext"), "outbound %s should have been dialed exactly once", tag)
	}
}

func TestMutableURLTest_DialContext_RetryCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// More outbounds than maxDialRetries so the cap is the binding constraint.
	tags := []string{"a", "b", "c", "d", "e"}
	outboundByTag := make(map[string]*mockOutbound, len(tags))
	g := &urlTestGroup{
		ctx:       ctx,
		logger:    sbLog.NewNOPFactory().Logger(),
		tags:      []string{},
		outbounds: sync.TypedMap[string, adapter.Outbound]{},
		tolerance: 50,
		history:   urltest.NewHistoryStorage(),
	}
	for i, tag := range tags {
		ob := &mockOutbound{tag: tag}
		g.tags = append(g.tags, tag)
		g.outbounds.Store(tag, ob)
		g.history.StoreURLTestHistory(tag, &adapter.URLTestHistory{
			Time:  time.Now(),
			Delay: uint16(100 * (i + 1)),
		})
		outboundByTag[tag] = ob
	}
	g.selectedOutboundTCP.Store(outboundByTag["a"])

	s := &MutableURLTest{
		Adapter: O.NewAdapter(constant.TypeMutableURLTest, "test", []string{"tcp", "udp"}, nil),
		ctx:     ctx,
		logger:  sbLog.NewNOPFactory().Logger(),
		group:   g,
	}
	for _, ob := range outboundByTag {
		ob.On("DialContext").Return(nil, assert.AnError)
	}

	conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
	assert.Error(t, err)
	assert.Nil(t, conn)
	assert.True(t, errors.Is(err, ErrAllOutboundsFailed),
		"err should be ErrAllOutboundsFailed; got: %v", err)

	total := 0
	for _, ob := range outboundByTag {
		total += callCount(ob, "DialContext")
	}
	assert.Equalf(t, maxDialRetries, total,
		"DialContext should have been attempted exactly maxDialRetries times across all outbounds; got %d", total)
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
