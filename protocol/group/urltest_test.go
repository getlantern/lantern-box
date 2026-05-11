package group

import (
	"fmt"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing/common"
	"github.com/stretchr/testify/assert"

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

	pfmt := func(o adapter.Outbound) string {
		if o == nil {
			return "<nil>"
		}
		h := testGroup.history.LoadURLTestHistory(o.Tag())
		if h != nil {
			return fmt.Sprintf("%s(%d)", o.Tag(), h.Delay)
		}
		return o.Tag()
	}

	o, _ := testGroup.outbounds.Load("golduck")
	testGroup.selectedOutboundTCP.Store(o)
	testGroup.selectedOutboundUDP.Store(o)

	fmt.Printf("Before updateSelected: tcp=%v, udp=%v\n",
		pfmt(testGroup.selectedOutboundTCP.Load()),
		pfmt(testGroup.selectedOutboundUDP.Load()),
	)
	testGroup.updateSelected()
	fmt.Printf("After updateSelected: tcp=%v, udp=%v\n",
		pfmt(testGroup.selectedOutboundTCP.Load()),
		pfmt(testGroup.selectedOutboundUDP.Load()),
	)

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

	picked := g.pickBestOutbound("tcp", alpha)
	assert.Equal(t, "beta", picked.Tag(),
		"fallback path should skip current and return the other outbound")
}
