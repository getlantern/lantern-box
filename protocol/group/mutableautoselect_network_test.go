package group

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

const (
	envA uint64 = 0xa
	envB uint64 = 0xb
)

type fakeNetworkManager struct {
	A.NetworkManager
	env atomic.Uint64
}

func (f *fakeNetworkManager) NetworkEnvironment() uint64 { return f.env.Load() }

// withNetwork attaches a fake network manager on env to s.
func withNetwork(s *MutableAutoSelect, env uint64) *fakeNetworkManager {
	nm := &fakeNetworkManager{}
	nm.env.Store(env)
	s.access.Lock()
	s.networkMgr = nm
	s.setEnvironmentLocked(env)
	s.access.Unlock()
	return nm
}

// switchNetwork moves the box to env the way sing-box does on a default
// interface change: the environment updates before InterfaceUpdated.
func switchNetwork(s *MutableAutoSelect, nm *fakeNetworkManager, env uint64) {
	nm.env.Store(env)
	s.InterfaceUpdated(context.Background())
}

func rankedFor(s *MutableAutoSelect, tag string) rankedCandidate {
	s.access.Lock()
	defer s.access.Unlock()
	for _, c := range s.rankLocked(time.Now()) {
		if c.tag == tag {
			return c
		}
	}
	return rankedCandidate{}
}

func TestNewMutableAutoSelect_ReadsNetworkEnvironment(t *testing.T) {
	nm := &fakeNetworkManager{}
	nm.env.Store(envA)
	ctx := service.ContextWith[A.NetworkManager](context.Background(), nm)
	out, err := NewMutableAutoSelect(ctx, nil, log.NewNOPFactory().Logger(), "group", option.MutableAutoSelectOutboundOptions{})
	require.NoError(t, err)
	s := out.(*MutableAutoSelect)
	t.Cleanup(func() { s.Close() })
	assert.Equal(t, envA, s.environment)
}

func TestHistory_DemotionIsPerNetwork(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	nm := withNetwork(s, envA)
	s.access.Lock()
	recordSuccess(s, "a", 50)
	recordSuccess(s, "b", 100)
	s.access.Unlock()
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	require.Equal(t, demoteHard, rankedFor(s, "a").demote, "failures on A hard-demote a there")

	switchNetwork(s, nm, envB)
	got := rankedFor(s, "a")
	assert.Equal(t, demoteClean, got.demote, "A's failures must not demote a on B")
	assert.Equal(t, uint32(50), got.delayMs, "a ranks on B by the delay last seen on A")
	assert.Zero(t, got.userFails)

	switchNetwork(s, nm, envA)
	assert.Equal(t, demoteHard, rankedFor(s, "a").demote, "returning to A restores its demotion")
}

func TestHistory_DelayHintPrefersMostRecentSuccess(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	now := time.Now()
	record := func(fn func(h *localHistory)) {
		require.True(t, s.mutateHistory("a", s.networkGeneration.Load(), func(h *localHistory, _ time.Time) bool {
			fn(h)
			return true
		}))
	}
	record(func(h *localHistory) { h.recordProbeSuccess(80, now.Add(-2*time.Minute)) })
	switchNetwork(s, nm, envB)
	record(func(h *localHistory) { h.recordProbeSuccess(30, now.Add(-time.Minute)) })
	switchNetwork(s, nm, envA)
	record(func(h *localHistory) { h.recordProbeFailure(now) })

	switchNetwork(s, nm, 0xc)
	assert.Equal(t, uint32(30), rankedFor(s, "a").delayMs,
		"a later probe failure on A must not make A's older success the hint")
}

func TestEnvironment_UnidentifiedNetworkKeepsItsHistory(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := &fakeNetworkManager{}
	s.networkMgr = nm
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	require.Equal(t, demoteHard, rankedFor(s, "a").demote)

	switchNetwork(s, nm, envB)
	assert.Equal(t, demoteClean, rankedFor(s, "a").demote,
		"failures on the unidentified network must not follow the member to B")
	switchNetwork(s, nm, 0)
	assert.Equal(t, demoteHard, rankedFor(s, "a").demote)
}

func TestEnvironment_ChangeRestoresSnapshots(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	s.hist.userFailureDedupeWindow = 0
	for range s.hist.consecutiveFailLimit {
		require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureStall, s.networkGeneration.Load()))
	}
	require.True(t, s.history.Load("a").HardDemoted)

	updatedOnA := s.history.Load("a").UpdatedAt

	switchNetwork(s, nm, envB)
	snap := s.history.Load("a")
	require.NotNil(t, snap)
	assert.False(t, snap.HardDemoted, "the stored view must follow the group to B")
	assert.Empty(t, snap.UserFailures)
	assert.True(t, snap.Networks[envA].HardDemoted, "A's history is kept")
	assert.Zero(t, snap.UpdatedAt, "UpdatedAt describes B, which has no history")
	assert.Equal(t, updatedOnA, snap.Networks[envA].UpdatedAt, "re-storing unchanged history must not refresh it")
}

func TestMutateHistory_DetectsEnvironmentChange(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	s.stickyTag.udp.Store("a")
	generation := s.networkGeneration.Load()

	nm.env.Store(envB)
	s.makeHooks("a", N.NetworkUDP, primaryRoute, generation).onFailure(adapter.UserFailureStall)

	assert.Greater(t, s.networkGeneration.Load(), generation)
	s.access.Lock()
	defer s.access.Unlock()
	assert.Equal(t, envB, s.environment)
	_, ok := s.histories["a"]
	assert.False(t, ok, "a failure observed before the change must not be recorded on either network")
}

func TestProbe_DetectsEnvironmentChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		nm := withNetwork(s, envA)
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
			}
			return probeResponderConn(), nil
		}
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()

		nm.env.Store(envB)
		close(release)
		synctest.Wait()

		s.access.Lock()
		defer s.access.Unlock()
		assert.Equal(t, envB, s.environment)
		_, onA := s.histories["a"].peek(envA)
		assert.False(t, onA, "a probe measured across the change must not be recorded on A")
	})
}

func TestSelectOutbound_DetectsEnvironmentChangeWithoutInterfaceUpdate(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	before := s.networkGeneration.Load()

	nm.env.Store(envB)
	_, generation, err := s.selectOutbound(N.NetworkTCP, "")
	require.NoError(t, err)
	assert.Greater(t, generation, before, "an environment change must advance the generation")
	assert.False(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, before),
		"a failure from the old environment's generation must be dropped")
	require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, generation))

	s.access.Lock()
	defer s.access.Unlock()
	assert.Equal(t, envB, s.environment)
	_, onA := s.histories["a"].peek(envA)
	assert.False(t, onA)
	h, onB := s.histories["a"].peek(envB)
	require.True(t, onB)
	assert.Equal(t, uint32(1), h.userFailureCount(time.Now(), s.hist.userFailureWindow))
}

func TestUserFailures_RecordNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newTestMUR(t, "a")
		s.cfg.ladderCooldown = time.Hour
		s.hist.userFailureDedupeWindow = 0
		dialErr := errors.New("dial failed")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, dialErr }
		obs["a"].On("ListenPacket").Return(nil, dialErr)

		_, err := s.DialContext(context.Background(), N.NetworkTCP, metadata.Socksaddr{})
		require.ErrorIs(t, err, dialErr)
		synctest.Wait()
		_, err = s.ListenPacket(context.Background(), metadata.Socksaddr{})
		require.ErrorIs(t, err, dialErr)
		synctest.Wait()
		s.stickyTag.udp.Store("a")
		s.makeHooks("a", N.NetworkUDP, primaryRoute, s.networkGeneration.Load()).onFailure(adapter.UserFailureStall)

		uf, ok := userFailures(s, "a")
		require.True(t, ok)
		require.Len(t, uf, 3)
		assert.Equal(t, N.NetworkTCP, uf[0].Network, "TCP dial failure")
		assert.Equal(t, N.NetworkUDP, uf[1].Network, "ListenPacket failure")
		assert.Equal(t, N.NetworkUDP, uf[2].Network, "UDP data-plane stall")
	})
}

func TestHistory_PersistsEveryNetwork(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	s.hist.userFailureDedupeWindow = 0
	for range s.hist.consecutiveFailLimit {
		require.True(t, s.recordUserFailure("a", N.NetworkUDP, adapter.UserFailureStall, s.networkGeneration.Load()))
	}
	switchNetwork(s, nm, envB)
	require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, s.networkGeneration.Load()))

	snap := s.history.Load("a")
	require.NotNil(t, snap)
	require.Len(t, snap.Networks, 2)
	assert.True(t, snap.Networks[envA].HardDemoted)
	assert.Len(t, snap.Networks[envA].UserFailures, int(s.hist.consecutiveFailLimit))
	assert.False(t, snap.Networks[envB].HardDemoted)
	assert.False(t, snap.HardDemoted, "the top level describes the current network, B")
	require.Len(t, snap.UserFailures, 1)
	assert.Equal(t, N.NetworkTCP, snap.UserFailures[0].Network)
}

func TestHydrateMemberHistory(t *testing.T) {
	now := time.Now()
	window := 5 * time.Minute
	maxAge := 15 * time.Minute

	t.Run("networks", func(t *testing.T) {
		snap := &adapter.TagHistory{
			LastSuccessDelayMs: 999, // ignored when Networks is present
			UpdatedAt:          now,
			Networks: map[uint64]*adapter.NetworkHistory{
				envA:  {LastSuccessDelayMs: 40, UpdatedAt: now.Add(-time.Minute)},
				envB:  {LastSuccessDelayMs: 60, UpdatedAt: now.Add(-time.Hour)},
				0xc:   nil,
				0xd00: {LastSuccessDelayMs: 70},
			},
		}
		m := hydrateMemberHistory(snap, envB, now, maxAge, window)
		a, ok := m.peek(envA)
		require.True(t, ok)
		delay, _, _, _ := a.snapshot(now, window)
		assert.Equal(t, uint32(40), delay)
		_, ok = m.peek(envB)
		assert.False(t, ok, "an entry older than maxAge is dropped")
		_, ok = m.peek(0xc)
		assert.False(t, ok)
		_, ok = m.peek(0xd00)
		assert.True(t, ok, "an entry without UpdatedAt is kept")
	})

	t.Run("flat seed goes to current network", func(t *testing.T) {
		snap := &adapter.TagHistory{LastSuccessDelayMs: 25, LastOutcomeAt: now, UpdatedAt: now}
		m := hydrateMemberHistory(snap, envB, now, maxAge, window)
		require.Len(t, m.byNetwork, 1)
		b, ok := m.peek(envB)
		require.True(t, ok)
		delay, _, _, _ := b.snapshot(now, window)
		assert.Equal(t, uint32(25), delay)
	})
}

func TestMemberHistory_PruneKeepsCurrentNetwork(t *testing.T) {
	now := time.Now()
	m := newMemberHistory()
	m.forNetwork(envA).updatedAt = now.Add(-time.Hour)
	m.forNetwork(envB).updatedAt = now.Add(-time.Hour)
	m.forNetwork(0xc).updatedAt = now
	m.prune(now, 15*time.Minute, envA)
	_, okA := m.peek(envA)
	_, okB := m.peek(envB)
	_, okC := m.peek(0xc)
	assert.True(t, okA, "the current network is never pruned")
	assert.False(t, okB)
	assert.True(t, okC)
}

func TestRankLocked_SeedFallbackUsesCurrentNetwork(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	withNetwork(s, envB)
	now := time.Now()
	s.history.Store("a", &adapter.TagHistory{
		UpdatedAt: now,
		Networks: map[uint64]*adapter.NetworkHistory{
			envA: {LastSuccessDelayMs: 40, LastOutcomeAt: now, ConsecutiveFailures: 9, UpdatedAt: now},
		},
	})
	s.history.Store("b", &adapter.TagHistory{
		UpdatedAt: now,
		Networks: map[uint64]*adapter.NetworkHistory{
			envB: {LastSuccessDelayMs: 90, LastOutcomeAt: now, ConsecutiveFailures: 9, UpdatedAt: now},
		},
	})
	a := rankedFor(s, "a")
	assert.Equal(t, uint32(40), a.delayMs, "another network's seed lends only its delay")
	assert.Equal(t, demoteClean, a.demote)
	assert.Equal(t, demoteHard, rankedFor(s, "b").demote, "the current network's seed applies in full")
}

func TestProbe_LastResortRecordedOnNetworkItMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		nm := withNetwork(s, envA)
		obs["a"].dial = denyDial
		release := make(chan struct{})
		obs["ub"].dial = func(context.Context) (net.Conn, error) { <-release; return probeResponderConn(), nil }
		s.CheckOutbounds()
		synctest.Wait()

		switchNetwork(s, nm, envB)
		synctest.Wait()
		close(release)
		synctest.Wait()

		s.access.Lock()
		defer s.access.Unlock()
		require.True(t, s.probeMembers["ub"].outcome.success, "a last-resort result still spans networks")
		onA, ok := s.histories["ub"].peek(envA)
		require.True(t, ok)
		delay, _, _, _ := onA.snapshot(time.Now(), s.hist.userFailureWindow)
		assert.Positive(t, delay, "the result is recorded on the network it was measured on")
		_, onB := s.histories["ub"].peek(envB)
		assert.False(t, onB, "a probe measured on A must not touch B's history")
	})
}

func TestHydrate_RestoresSnapshotForCurrentNetwork(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	withNetwork(s, envB)
	updated := time.Now().Add(-time.Minute)
	s.history.Store("a", &adapter.TagHistory{
		LastSuccessDelayMs: 40,
		HardDemoted:        true,
		UpdatedAt:          updated,
		Networks: map[uint64]*adapter.NetworkHistory{
			envA: {LastSuccessDelayMs: 40, HardDemoted: true, ConsecutiveFailures: 9, UpdatedAt: updated},
		},
	})
	s.access.Lock()
	s.hydrateHistoryLocked("a")
	s.access.Unlock()

	snap := s.history.Load("a")
	require.NotNil(t, snap)
	assert.False(t, snap.HardDemoted, "the stored view must describe the current network, B")
	assert.Zero(t, snap.LastSuccessDelayMs)
	assert.True(t, snap.Networks[envA].HardDemoted, "A's history is kept")
	assert.Zero(t, snap.UpdatedAt, "UpdatedAt describes B, which has no history")
	assert.True(t, snap.Networks[envA].UpdatedAt.Equal(updated), "re-storing on hydrate must not refresh it")
}

func TestProbe_DispatchRefreshesEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		nm := withNetwork(s, envA)
		obs["a"].dial = denyDial
		obs["ub"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }

		nm.env.Store(envB)
		s.CheckOutbounds()
		synctest.Wait()

		s.access.Lock()
		defer s.access.Unlock()
		assert.Equal(t, envB, s.environment)
		_, onB := s.histories["ub"].peek(envB)
		assert.True(t, onB, "a probe dispatched after the change measures and records on B")
		_, onA := s.histories["ub"].peek(envA)
		assert.False(t, onA)
	})
}

func TestUserFailures_DedupeSpansTCPAndUDP(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, s.networkGeneration.Load()))
	assert.False(t, s.recordUserFailure("a", N.NetworkUDP, adapter.UserFailureDial, s.networkGeneration.Load()),
		"a UDP failure right after a TCP one is the same event")
}

func TestUserFailures_TCPAndUDPDemoteTogether(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.hist.userFailureDedupeWindow = 0
	networks := []string{N.NetworkTCP, N.NetworkUDP, N.NetworkTCP}
	require.Len(t, networks, int(s.hist.consecutiveFailLimit))
	for _, network := range networks {
		require.True(t, s.recordUserFailure("a", network, adapter.UserFailureStall, s.networkGeneration.Load()))
	}
	assert.Equal(t, demoteHard, rankedFor(s, "a").demote)
	assert.True(t, s.history.Load("a").HardDemoted)
}

func TestEnvironment_TopLevelUpdatedAtIsCurrentNetworks(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envB)
	s.hist.userFailureDedupeWindow = 0
	require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, s.networkGeneration.Load()))
	updatedOnB := s.history.Load("a").UpdatedAt
	switchNetwork(s, nm, envA)
	require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, s.networkGeneration.Load()))
	require.True(t, s.history.Load("a").UpdatedAt.After(updatedOnB))

	switchNetwork(s, nm, envB)
	assert.Equal(t, updatedOnB, s.history.Load("a").UpdatedAt,
		"B's view must carry B's timestamp, not A's newer one")
}

func TestHydrate_RefreshesEnvironmentFirst(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	now := time.Now()
	s.history.Store("a", &adapter.TagHistory{LastSuccessDelayMs: 25, LastOutcomeAt: now, UpdatedAt: now})

	nm.env.Store(envB)
	s.access.Lock()
	s.hydrateHistoryLocked("a")
	_, onB := s.histories["a"].peek(envB)
	_, onA := s.histories["a"].peek(envA)
	s.access.Unlock()
	assert.True(t, onB, "a flat snapshot belongs to the network current when it loads")
	assert.False(t, onA)
}

func TestHydrate_StaleCurrentNetworkKeepsOthers(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	withNetwork(s, envB)
	now := time.Now()
	s.history.Store("a", &adapter.TagHistory{
		UpdatedAt: now.Add(-time.Hour),
		Networks: map[uint64]*adapter.NetworkHistory{
			envA: {LastSuccessDelayMs: 40, LastSuccessAt: now, UpdatedAt: now},
			envB: {LastSuccessDelayMs: 90, ConsecutiveFailures: 9, UpdatedAt: now.Add(-time.Hour)},
		},
	})
	s.access.Lock()
	s.hydrateHistoryLocked("a")
	s.access.Unlock()
	got := rankedFor(s, "a")
	assert.Equal(t, demoteClean, got.demote, "B's stale failures are dropped")
	assert.Equal(t, uint32(40), got.delayMs, "A's fresh delay survives a stale snapshot timestamp")
}

func TestHydrate_RoundTripRanksByLatestSuccess(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	now := time.Now()
	record := func(fn func(h *localHistory, now time.Time)) {
		require.True(t, s.mutateHistory("a", s.networkGeneration.Load(), func(h *localHistory, now time.Time) bool {
			fn(h, now)
			return true
		}))
	}
	record(func(h *localHistory, _ time.Time) { h.recordProbeSuccess(80, now.Add(-2*time.Minute)) })
	switchNetwork(s, nm, envB)
	record(func(h *localHistory, now time.Time) { h.recordProbeSuccess(30, now) })
	switchNetwork(s, nm, envA)
	record(func(h *localHistory, now time.Time) { h.recordProbeFailure(now) })
	data, err := json.Marshal(s.history.Load("a"))
	require.NoError(t, err)

	fresh, _ := newTestMUR(t, "a")
	withNetwork(fresh, 0xc)
	var snap adapter.TagHistory
	require.NoError(t, json.Unmarshal(data, &snap))
	fresh.history.Store("a", &snap)
	fresh.access.Lock()
	fresh.hydrateHistoryLocked("a")
	fresh.access.Unlock()
	assert.Equal(t, uint32(30), rankedFor(fresh, "a").delayMs)
}

func TestProbe_LastResortFailureRecordedOnNetworkItMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		nm := withNetwork(s, envA)
		obs["a"].dial = denyDial
		release := make(chan struct{})
		obs["ub"].dial = func(context.Context) (net.Conn, error) { <-release; return nil, errors.New("failed") }
		s.CheckOutbounds()
		synctest.Wait()

		switchNetwork(s, nm, envB)
		synctest.Wait()
		close(release)
		synctest.Wait()

		s.access.Lock()
		defer s.access.Unlock()
		outcome := s.probeMembers["ub"].outcome
		require.NotNil(t, outcome)
		assert.False(t, outcome.success)
		assert.True(t, s.outcomeCurrentLocked("ub", outcome), "a last-resort failure still counts on B for scheduling")
		onA, ok := s.histories["ub"].peek(envA)
		require.True(t, ok)
		_, consec, _, _ := onA.snapshot(time.Now(), s.hist.userFailureWindow)
		assert.Equal(t, uint32(1), consec)
		_, onB := s.histories["ub"].peek(envB)
		assert.False(t, onB, "a failure measured on A must not touch B's history")
	})
}

func TestRankLocked_SeedSkipsStaleNetworks(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	withNetwork(s, envB)
	now := time.Now()
	s.history.Store("a", &adapter.TagHistory{
		Networks: map[uint64]*adapter.NetworkHistory{
			envA:  {LastSuccessDelayMs: 40, LastSuccessAt: now.Add(-time.Hour), UpdatedAt: now},
			envB:  {LastSuccessDelayMs: 90, ConsecutiveFailures: 9, UpdatedAt: now.Add(-time.Hour)},
			0xc00: {LastSuccessDelayMs: 10, LastSuccessAt: now, UpdatedAt: now.Add(-time.Hour)},
		},
	})
	got := rankedFor(s, "a")
	assert.Equal(t, demoteClean, got.demote, "the current network's stale entry gives nothing")
	assert.Equal(t, uint32(40), got.delayMs, "a stale network can't supply the hint")
}

func TestEnvironment_ChangeExpiresIdleNetworks(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	s.hist.userFailureDedupeWindow = 0
	for range s.hist.consecutiveFailLimit {
		require.True(t, s.mutateHistory("a", s.networkGeneration.Load(), func(h *localHistory, now time.Time) bool {
			h.recordProbeFailure(now)
			return true
		}))
	}
	require.Equal(t, demoteHard, rankedFor(s, "a").demote)
	switchNetwork(s, nm, envB)

	// Age A's history past max_persisted_age with no writes since.
	s.access.Lock()
	onA, ok := s.histories["a"].peek(envA)
	require.True(t, ok)
	onA.updatedAt = onA.updatedAt.Add(-2 * s.cfg.maxPersistedAge)
	s.access.Unlock()

	switchNetwork(s, nm, envA)
	assert.Equal(t, demoteClean, rankedFor(s, "a").demote, "A's expired failures must not come back")
	s.access.Lock()
	_, ok = s.histories["a"].peek(envA)
	s.access.Unlock()
	assert.False(t, ok)
	assert.Nil(t, s.history.Load("a"), "a member with no unexpired history is removed from storage")
}

func TestMemberHistory_PruneCapsNetworks(t *testing.T) {
	now := time.Now()
	m := newMemberHistory()
	// The kept network is the oldest, so the cap must skip it.
	m.forNetwork(0).updatedAt = now.Add(-5 * time.Minute)
	for env := uint64(1); env <= maxNetworksPerMember+1; env++ {
		m.forNetwork(env).updatedAt = now.Add(-time.Duration(maxNetworksPerMember+2-env) * time.Second)
	}
	m.prune(now, time.Hour, 0)
	require.Len(t, m.byNetwork, maxNetworksPerMember)
	_, kept := m.peek(0)
	assert.True(t, kept, "the kept network survives the cap")
	for env := uint64(1); env <= 2; env++ {
		_, ok := m.peek(env)
		assert.False(t, ok, "the least recently updated others go first")
	}
}

func TestHistory_NetworksCappedAcrossSwitches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a")
		nm := withNetwork(s, 1)
		s.hist.userFailureDedupeWindow = 0
		for env := uint64(1); env <= maxNetworksPerMember+1; env++ {
			switchNetwork(s, nm, env)
			require.True(t, s.recordUserFailure("a", N.NetworkTCP, adapter.UserFailureDial, s.networkGeneration.Load()))
			time.Sleep(time.Second)
		}
		snap := s.history.Load("a")
		require.Len(t, snap.Networks, maxNetworksPerMember)
		_, first := snap.Networks[1]
		assert.False(t, first, "the network used longest ago is dropped")
		_, current := snap.Networks[maxNetworksPerMember+1]
		assert.True(t, current)
	})
}

func TestHydrateMemberHistory_CapsNetworks(t *testing.T) {
	now := time.Now()
	snap := &adapter.TagHistory{Networks: map[uint64]*adapter.NetworkHistory{}}
	for env := uint64(1); env <= maxNetworksPerMember+2; env++ {
		snap.Networks[env] = &adapter.NetworkHistory{UpdatedAt: now.Add(-time.Duration(maxNetworksPerMember+3-env) * time.Second)}
	}
	// The current network is the oldest entry and must survive.
	m := hydrateMemberHistory(snap, 1, now, time.Hour, 5*time.Minute)
	require.Len(t, m.byNetwork, maxNetworksPerMember)
	_, ok := m.peek(1)
	assert.True(t, ok)
	for env := uint64(2); env <= 3; env++ {
		_, ok := m.peek(env)
		assert.False(t, ok)
	}
}

func TestEnvironment_LeavingExpiredNetworkDropsIt(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	s.access.Lock()
	recordSuccess(s, "a", 40)
	onA, _ := s.histories["a"].peek(envA)
	onA.updatedAt = onA.updatedAt.Add(-2 * s.cfg.maxPersistedAge)
	s.access.Unlock()

	switchNetwork(s, nm, envB)
	s.access.Lock()
	_, ok := s.histories["a"].peek(envA)
	s.access.Unlock()
	assert.False(t, ok, "an expired network is dropped when the group leaves it")
	assert.Zero(t, rankedFor(s, "a").delayMs, "an expired network can't supply the hint")
}

func TestMemberHistory_DelayHintSkipsExpired(t *testing.T) {
	now := time.Now()
	m := newMemberHistory()
	stale := m.forNetwork(envA)
	stale.recordProbeSuccess(10, now)
	stale.updatedAt = now.Add(-time.Hour)
	fresh := m.forNetwork(envB)
	fresh.recordProbeSuccess(50, now.Add(-time.Minute))
	fresh.updatedAt = now.Add(-time.Minute)
	assert.Equal(t, uint32(50), m.delayHint(0xc, now, 15*time.Minute))
	assert.Equal(t, uint32(10), m.delayHint(0xc, now, 0), "maxAge <= 0 disables aging")
}

// ageNetwork makes tag's history on env look last changed age ago.
func ageNetwork(t *testing.T, s *MutableAutoSelect, tag string, env uint64, age time.Duration) {
	t.Helper()
	s.access.Lock()
	defer s.access.Unlock()
	h, ok := s.histories[tag].peek(env)
	require.True(t, ok)
	h.updatedAt = time.Now().Add(-age)
}

func failProbes(t *testing.T, s *MutableAutoSelect, tag string, env uint64, n uint32) {
	t.Helper()
	s.access.Lock()
	defer s.access.Unlock()
	for range n {
		require.True(t, s.mutateHistoryLocked(tag, env, func(h *localHistory, now time.Time) bool {
			h.recordProbeFailure(now)
			return true
		}))
	}
}

func TestMutateHistory_LateResultOnExpiredNetworkStartsFresh(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	nm := withNetwork(s, envA)
	failProbes(t, s, "a", envA, s.hist.consecutiveFailLimit-1)
	switchNetwork(s, nm, envB)
	ageNetwork(t, s, "a", envA, 2*s.cfg.maxPersistedAge)

	// A last-resort result measured on A lands after A expired.
	failProbes(t, s, "a", envA, 1)
	s.access.Lock()
	onA, ok := s.histories["a"].peek(envA)
	require.True(t, ok)
	_, consec, _, _ := onA.snapshot(time.Now(), s.hist.userFailureWindow)
	s.access.Unlock()
	assert.Equal(t, uint32(1), consec, "expired counters must not be revived")
	assert.False(t, s.history.Load("a").Networks[envA].HardDemoted)
}

func TestMutateHistory_ExpiresOnlyThatMembersIdleNetworks(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	nm := withNetwork(s, envA)
	failProbes(t, s, "a", envA, 1)
	failProbes(t, s, "b", envA, 1)
	switchNetwork(s, nm, envB)
	ageNetwork(t, s, "a", envA, 2*s.cfg.maxPersistedAge)
	ageNetwork(t, s, "b", envA, 2*s.cfg.maxPersistedAge)

	failProbes(t, s, "a", envB, 1)
	_, aOnA := s.history.Load("a").Networks[envA]
	assert.False(t, aOnA, "a write expires the member's own idle networks in storage")
	s.access.Lock()
	_, aMem := s.histories["a"].peek(envA)
	_, bMem := s.histories["b"].peek(envA)
	s.access.Unlock()
	assert.False(t, aMem)
	assert.True(t, bMem, "another member's history is left to its own writes and network changes")
	assert.Zero(t, rankedFor(s, "b").delayMs, "but its expired network can't feed ranking")
}

func TestHistory_CurrentNetworkNeverExpires(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	withNetwork(s, envA)
	failProbes(t, s, "a", envA, s.hist.consecutiveFailLimit)
	ageNetwork(t, s, "a", envA, 2*s.cfg.maxPersistedAge)

	failProbes(t, s, "b", envA, 1)
	assert.Equal(t, demoteHard, rankedFor(s, "a").demote,
		"the current network's history stays in force however long ago it changed")
	failProbes(t, s, "a", envA, 1)
	assert.True(t, s.history.Load("a").HardDemoted)
}

func TestEnvironment_ChangeSweepsEveryMember(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	nm := withNetwork(s, envA)
	failProbes(t, s, "a", envA, s.hist.consecutiveFailLimit)
	failProbes(t, s, "b", envA, s.hist.consecutiveFailLimit)
	switchNetwork(s, nm, envB)
	failProbes(t, s, "b", envB, 1)
	ageNetwork(t, s, "a", envA, 2*s.cfg.maxPersistedAge)
	ageNetwork(t, s, "b", envA, 2*s.cfg.maxPersistedAge)

	switchNetwork(s, nm, 0xc)
	assert.Nil(t, s.history.Load("a"), "a member left with nothing is removed")
	snapB := s.history.Load("b")
	require.NotNil(t, snapB)
	_, bOnA := snapB.Networks[envA]
	assert.False(t, bOnA)
	assert.Contains(t, snapB.Networks, envB)
	assert.Zero(t, snapB.ConsecutiveFailures, "b's stored view follows the destination")
	assert.False(t, snapB.HardDemoted)
}

func TestMutateHistory_LateResultAtCapKeepsCurrentAndWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a")
		withNetwork(s, 1)
		// The current network 1 is the oldest; 2..4 follow.
		for env := uint64(1); env <= maxNetworksPerMember; env++ {
			failProbes(t, s, "a", env, 1)
			time.Sleep(time.Second)
		}
		// A late result measured on network 5 makes a fifth entry.
		failProbes(t, s, "a", maxNetworksPerMember+1, 1)

		s.access.Lock()
		got := slices.Sorted(maps.Keys(s.histories["a"].byNetwork))
		s.access.Unlock()
		want := []uint64{1, 3, 4, maxNetworksPerMember + 1}
		assert.Equal(t, want, got, "the oldest network other than current and written is evicted")
		assert.Equal(t, want, slices.Sorted(maps.Keys(s.history.Load("a").Networks)))
	})
}

func TestProbeCycle_RefreshesEnvironmentBeforeWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		nm := withNetwork(s, envA)
		obs["a"].dial = denyDial

		nm.env.Store(envB)
		s.runProbeCycle()
		synctest.Wait()

		assert.True(t, exhausted(s), "the cycle must still evaluate on the new network")
		s.access.Lock()
		defer s.access.Unlock()
		_, onB := s.histories["a"].peek(envB)
		assert.True(t, onB)
		_, onA := s.histories["a"].peek(envA)
		assert.False(t, onA)
	})
}

func TestLadder_EnvironmentChangeBeforeBudgetDiscardsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		nm := withNetwork(s, envA)
		release := make(chan struct{})
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("failed")
		}
		done := make(chan struct{})
		go func() {
			s.runLadder("a", s.networkGeneration.Load())
			close(done)
		}()
		synctest.Wait()

		nm.env.Store(envB)
		time.Sleep(s.cfg.ladderTotalBudget + time.Millisecond)
		synctest.Wait()
		assert.False(t, exhausted(s), "a ladder from the previous network must not signal exhaustion")
		close(release)
		<-done
	})
}

func TestRecoveryGate_IsPerNetwork(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	nm := withNetwork(s, envA)
	s.access.Lock()
	recordSuccess(s, "a", 50)
	recordSuccess(s, "b", 60)
	s.historyForLocked("a").startRecoveryGate(time.Now())
	s.access.Unlock()
	require.True(t, rankedFor(s, "a").hasRecoveryGate)

	switchNetwork(s, nm, envB)
	assert.False(t, rankedFor(s, "a").hasRecoveryGate, "A's gate doesn't apply on B")
	switchNetwork(s, nm, envA)
	assert.True(t, rankedFor(s, "a").hasRecoveryGate, "A's gate is still there on return")
}

// lateLastResortProbe starts a last-resort probe of "ub" on the current
// network, switches to next before it completes, runs between, then lets the
// probe fail.
func lateLastResortProbe(t *testing.T, s *MutableAutoSelect, obs map[string]*mockOutbound, nm *fakeNetworkManager, next uint64, between func()) {
	t.Helper()
	s.defaultURL = "http://probe.test/"
	obs["a"].dial = denyDial
	release := make(chan struct{})
	obs["ub"].dial = func(context.Context) (net.Conn, error) { <-release; return nil, errors.New("failed") }
	s.CheckOutbounds()
	synctest.Wait()
	switchNetwork(s, nm, next)
	synctest.Wait()
	between()
	close(release)
	synctest.Wait()
}

func TestProbe_LateLastResortAfterExpiryStartsFresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		nm := withNetwork(s, envA)
		failProbes(t, s, "ub", envA, s.hist.consecutiveFailLimit-1)
		lateLastResortProbe(t, s, obs, nm, envB, func() {
			ageNetwork(t, s, "ub", envA, 2*s.cfg.maxPersistedAge)
		})

		s.access.Lock()
		defer s.access.Unlock()
		onA, ok := s.histories["ub"].peek(envA)
		require.True(t, ok)
		_, consec, _, _ := onA.snapshot(time.Now(), s.hist.userFailureWindow)
		assert.Equal(t, uint32(1), consec, "the late failure starts A afresh")
	})
}

func TestProbe_LateLastResortAtCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		nm := withNetwork(s, 1)
		for env := uint64(1); env <= maxNetworksPerMember; env++ {
			failProbes(t, s, "ub", env, 1)
			time.Sleep(time.Second)
		}
		// The probe runs on network 5 and completes after the switch to 6.
		switchNetwork(s, nm, maxNetworksPerMember+1)
		lateLastResortProbe(t, s, obs, nm, maxNetworksPerMember+2, func() {})

		want := []uint64{2, 3, 4, maxNetworksPerMember + 1}
		s.access.Lock()
		got := slices.Sorted(maps.Keys(s.histories["ub"].byNetwork))
		s.access.Unlock()
		assert.Equal(t, want, got, "the late result is kept and the oldest network evicted")
		assert.Equal(t, want, slices.Sorted(maps.Keys(s.history.Load("ub").Networks)))
	})
}
