package group

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sbAdapter "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
	lConst "github.com/getlantern/lantern-box/constant"
)

// Type returns the protocol type used by behaviorFor; an empty
// typeName keeps the default (non-excluded, 2s probe timeout), which is
// the right shape for selection-logic tests that don't care about
// per-protocol knobs. Set typeName to drive substituteDelay / excluded
// branches.
func (m *mockOutbound) Type() string { return m.typeName }

// newTestMUR builds a minimal MutableAutoSelect populated with the given
// tags backed by mockOutbounds. Members are pre-loaded so callers don't
// need an outboundMgr; the bg loop is not started.
func newTestMUR(t *testing.T, tags ...string) (*MutableAutoSelect, map[string]*mockOutbound) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &MutableAutoSelect{
		ctx:          ctx,
		cancel:       cancel,
		logger:       log.NewNOPFactory().Logger(),
		tags:         append([]string(nil), tags...),
		urlOverrides: map[string]string{},
		histories:    map[string]*localHistory{},
		cfg: mutableAutoSelectConfig{
			switchTolerance:   50 * time.Millisecond,
			activeInterval:    time.Hour,
			idleInterval:      4 * time.Hour,
			idleThreshold:     10 * time.Minute,
			ladderFastRetry:   50 * time.Millisecond,
			ladderTotalBudget: 100 * time.Millisecond,
			dataPlaneIdle:     time.Hour,
		},
		hist:         defaultHistoryParams(),
		history:      adapter.NewAutoSelectHistoryStorage(),
		exhaustionCh: make(chan struct{}, 1),
	}
	obs := make(map[string]*mockOutbound, len(tags))
	for _, tag := range tags {
		ob := &mockOutbound{tag: tag}
		obs[tag] = ob
		s.members.Store(tag, sbAdapter.Outbound(ob))
	}
	return s, obs
}

// recordSuccess writes a single fresh success entry to a member's history,
// returning the timestamp so callers can compute a cycleStart that
// includes (or excludes) the entry.
func recordSuccess(s *MutableAutoSelect, tag string, delay uint32) time.Time {
	now := time.Now()
	h := s.historyForLocked(tag)
	h.append(outcomeSuccess, delay, now)
	return now
}

func TestRecentSuccessRate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		entries   []timestampedOutcome
		wantTotal uint32
		wantRate  float64
	}{
		{
			name: "mixed window with one outside",
			entries: []timestampedOutcome{
				{outcome: outcomeSuccess, timestamp: now.Add(-10 * time.Minute)},
				{outcome: outcomeSuccess, timestamp: now.Add(-20 * time.Minute)},
				{outcome: outcomeTimeout, timestamp: now.Add(-30 * time.Minute)},
				{outcome: outcomeSuccess, timestamp: now.Add(-2 * time.Hour)}, // outside window
			},
			wantTotal: 3,
			wantRate:  2.0 / 3.0,
		},
		{
			name: "future-stamped entry clamps to age 0",
			entries: []timestampedOutcome{
				{outcome: outcomeSuccess, timestamp: now.Add(time.Hour)},
			},
			wantTotal: 1,
			wantRate:  1.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate, total := recentSuccessRate(tt.entries, now, time.Hour)
			assert.Equal(t, tt.wantTotal, total)
			assert.InDelta(t, tt.wantRate, rate, 1e-9)
		})
	}
}

func TestDemoted(t *testing.T) {
	now := time.Now()
	p := defaultHistoryParams()
	belowRate := []timestampedOutcome{
		{outcome: outcomeTimeout, timestamp: now.Add(-1 * time.Minute)},
		{outcome: outcomeTimeout, timestamp: now.Add(-2 * time.Minute)},
		{outcome: outcomeTimeout, timestamp: now.Add(-3 * time.Minute)},
		{outcome: outcomeSuccess, timestamp: now.Add(-4 * time.Minute)},
	}
	atRate := append([]timestampedOutcome(nil), belowRate...)
	atRate = append(atRate, timestampedOutcome{outcome: outcomeTimeout, timestamp: now.Add(-5 * time.Minute)})

	tests := []struct {
		name      string
		entries   []timestampedOutcome
		consec    uint32
		userFails uint32
		want      bool
	}{
		{"consecutive at limit", nil, p.consecutiveFailLimit, 0, true},
		{"consecutive below limit", nil, p.consecutiveFailLimit - 1, 0, false},
		{"user failures at limit", nil, 0, p.consecutiveFailLimit, true},
		{"user failures below limit", nil, 0, p.consecutiveFailLimit - 1, false},
		{"rate below min outcomes", belowRate, 0, 0, false},
		{"rate at min outcomes", atRate, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, demoted(tt.entries, tt.consec, tt.userFails, now, p))
		})
	}
}

func TestLocalHistory_RingBufferEvictsOldest(t *testing.T) {
	h := newLocalHistory(defaultHistoryRingMax)
	now := time.Now()
	for i := 0; i < defaultHistoryRingMax+10; i++ {
		o := outcomeSuccess
		if i%2 == 0 {
			o = outcomeTimeout
		}
		h.append(o, 100, now.Add(time.Duration(i)*time.Second))
	}
	entries, _, _ := h.snapshot()
	require.Lenf(t, entries, defaultHistoryRingMax,
		"ring buffer should cap at %d", defaultHistoryRingMax)
	expectedFirst := now.Add(10 * time.Second)
	assert.Falsef(t, entries[0].timestamp.Before(expectedFirst),
		"oldest entry not evicted: first ts=%v want>=%v", entries[0].timestamp, expectedFirst)
}

func TestLocalHistory_ConsecutiveFailuresResetOnSuccess(t *testing.T) {
	h := newLocalHistory(defaultHistoryRingMax)
	now := time.Now()
	h.append(outcomeTimeout, 0, now)
	h.append(outcomeHandshakeFailure, 0, now.Add(time.Second))
	_, consec, _ := h.snapshot()
	assert.Equal(t, uint32(2), consec, "two failures should accumulate")
	h.append(outcomeSuccess, 100, now.Add(2*time.Second))
	_, consec, _ = h.snapshot()
	assert.Equal(t, uint32(0), consec, "success should reset consecutive failures")
}

func TestLocalHistory_UserFailuresAreIndependentOfProbeOutcomes(t *testing.T) {
	// Regression: a probe success must not reset the user-failure counter.
	// This is the core anti-laundering invariant.
	h := newLocalHistory(defaultHistoryRingMax)
	now := time.Now()
	h.bumpUserFailures()
	h.bumpUserFailures()
	assert.Equal(t, uint32(2), h.userFailureCount(),
		"user failures should accumulate")
	h.append(outcomeSuccess, 100, now)
	assert.Equal(t, uint32(2), h.userFailureCount(),
		"probe success must NOT reset the user-failure counter")
	h.resetUserFailures()
	assert.Equal(t, uint32(0), h.userFailureCount(),
		"explicit reset (via successful Read) clears the counter")
}

func TestHasOutcomeSince(t *testing.T) {
	cutoff := time.Now()
	entries := []timestampedOutcome{
		{outcome: outcomeSuccess, timestamp: cutoff.Add(-time.Minute)},
	}
	assert.False(t, hasOutcomeSince(entries, cutoff),
		"entry before cutoff should not be counted")
	entries = append(entries, timestampedOutcome{outcome: outcomeSuccess, timestamp: cutoff.Add(time.Millisecond)})
	assert.True(t, hasOutcomeSince(entries, cutoff),
		"entry at/after cutoff should be counted")
}

func TestLastSuccessDelay(t *testing.T) {
	entries := []timestampedOutcome{
		{outcome: outcomeSuccess, delayMs: 100},
		{outcome: outcomeTimeout},
		{outcome: outcomeSuccess, delayMs: 250},
		{outcome: outcomeHandshakeFailure},
	}
	assert.Equal(t, uint32(250), lastSuccessDelay(entries))
	assert.Equal(t, uint32(0), lastSuccessDelay(nil), "empty history returns 0")
}

func TestBehaviorFor_Timeouts(t *testing.T) {
	cases := []struct {
		typeName string
		want     time.Duration
	}{
		{lConst.TypeALGeneva, 3000 * time.Millisecond},
		{lConst.TypeAmnezia, 1500 * time.Millisecond},
		{C.TypeHTTP, 2000 * time.Millisecond},
		{C.TypeHysteria, 1500 * time.Millisecond},
		{C.TypeHysteria2, 1500 * time.Millisecond},
		{lConst.TypeOutline, 10000 * time.Millisecond},
		{lConst.TypeReflex, 3000 * time.Millisecond},
		{lConst.TypeSamizdat, 3000 * time.Millisecond},
		{C.TypeShadowsocks, 2000 * time.Millisecond},
		{C.TypeShadowTLS, 3000 * time.Millisecond},
		{C.TypeSOCKS, 2000 * time.Millisecond},
		{C.TypeSSH, 3000 * time.Millisecond},
		{C.TypeTrojan, 3000 * time.Millisecond},
		{C.TypeTUIC, 1500 * time.Millisecond},
		{C.TypeVLESS, 2000 * time.Millisecond},
		{C.TypeVMess, 2000 * time.Millisecond},
		{C.TypeWireGuard, 1500 * time.Millisecond},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, behaviorFor(c.typeName).probeTimeout,
			"behaviorFor(%q).probeTimeout", c.typeName)
	}
}

func TestBehaviorFor_PeerNetworkProtocolsAreExcluded(t *testing.T) {
	for _, typeName := range []string{C.TypeTor, lConst.TypeUnbounded} {
		assert.Truef(t, behaviorFor(typeName).excludeFromPool,
			"%s should be excluded from candidate pool", typeName)
	}
}

func TestBehaviorFor_SamizdatGetsSubstituteDelay(t *testing.T) {
	beh := behaviorFor(lConst.TypeSamizdat)
	assert.Equal(t, samizdatNominalDelay, beh.substituteDelay)
}

// --- selectFor: switch tolerance via sticky-tag hysteresis ---

func TestSelectFor_ThreeTierFallback(t *testing.T) {
	// Tier ordering: clean > soft > hard. Each row picks the cleanest
	// non-empty tier; ties within a tier go to lowest delay.
	tests := []struct {
		name    string
		setup   func(s *MutableAutoSelect)
		wantTag string
	}{
		{
			name: "soft-demoted loses to clean peer",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				recordSuccess(s, "b", 200)
				s.historyForLocked("a").bumpUserFailures()
			},
			wantTag: "b",
		},
		{
			name: "soft-only pool returns the soft member",
			setup: func(s *MutableAutoSelect) {
				s.historyForLocked("a").bumpUserFailures()
				s.historyForLocked("b").bumpUserFailures()
			},
			wantTag: "a",
		},
		{
			name: "all-hard falls through to lowest-delay hard",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				recordSuccess(s, "b", 200)
				for i := 0; i < int(s.hist.consecutiveFailLimit); i++ {
					s.historyForLocked("a").bumpUserFailures()
					s.historyForLocked("b").bumpUserFailures()
				}
			},
			wantTag: "a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "a", "b")
			s.access.Lock()
			tt.setup(s)
			s.access.Unlock()
			got, err := s.selectFor("tcp")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantTag, got.Tag())
		})
	}
}

func TestSelectFor_SwitchTolerance(t *testing.T) {
	// switchTolerance is 50ms (test default).
	tests := []struct {
		name           string
		alphaDelay     uint32
		betaDelay      uint32
		want           string
		commentary     string
	}{
		{"sticky kept within tolerance", 100, 80, "alpha", "80+50 > 100 → keep alpha"},
		{"switches when beats tolerance", 100, 40, "beta", "40+50 <= 100 → switch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "alpha", "beta")
			recordSuccess(s, "alpha", tt.alphaDelay)
			recordSuccess(s, "beta", tt.betaDelay)
			s.stickyTag.tcp.Store("alpha")
			got, err := s.selectFor("tcp")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.Tag(), tt.commentary)
		})
	}
}

func TestSelectFor_ForcedSwitchWhenStickyNotInPool(t *testing.T) {
	// alpha is sticky but has no fresh success and gets pushed into the
	// hard tier; beta is clean. The hard-tier alpha is filtered out of
	// the clean pool, so the forced-switch branch fires.
	s, _ := newTestMUR(t, "alpha", "beta")
	recordSuccess(s, "beta", 120)
	// Push alpha to hard-demote: enough user failures to cross the limit.
	for i := 0; i < int(s.hist.consecutiveFailLimit); i++ {
		s.access.Lock()
		s.historyForLocked("alpha").bumpUserFailures()
		s.access.Unlock()
	}
	s.stickyTag.tcp.Store("alpha")

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "beta", got.Tag(),
		"sticky alpha demoted to hard tier; clean beta wins the forced switch")
}

func TestSelectFor_RecordsStickyAfterPick(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	recordSuccess(s, "a", 50)
	_, err := s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "a", loadString(&s.stickyTag.tcp),
		"selectFor should record the picked tag as sticky for future calls")
}

func TestPeekHistoryLocked_DoesNotCreateEntry(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	_, ok := s.peekHistoryLocked("a")
	assert.False(t, ok, "peek must not create a history entry")
	_, present := s.histories["a"]
	assert.False(t, present, "peek leaked an empty history entry into the map")
}

// --- Add / Remove invariants ---

func TestRemove_PreservesTagOrder(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c", "d")
	n, err := s.Remove("b")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, []string{"a", "c", "d"}, s.All())
}

func TestAdd_DoesNotDuplicateAlreadyListedTag(t *testing.T) {
	// Simulate a post-Start-failure inconsistency: tag is in s.tags but
	// missing from s.members. Add must repopulate members without
	// appending a duplicate to s.tags.
	s, _ := newTestMUR(t, "a")
	mgr := &mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{
		"a": &mockOutbound{tag: "a"},
	}}
	s.outboundMgr = mgr
	s.members.Delete("a") // drop members but leave tags intact
	n, err := s.Add("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, []string{"a"}, s.All(), "Add must not duplicate the already-listed tag")
}

// --- SetURLOverrides invalidates history on removed override ---

func TestSetURLOverrides_RemovingOverrideInvalidatesHistory(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	s.urlOverrides = map[string]string{"a": "https://override.example/a"}
	recordSuccess(s, "a", 100)
	recordSuccess(s, "b", 200)

	s.SetURLOverrides(map[string]string{}) // no overrides at all

	_, aPresent := s.histories["a"]
	assert.False(t, aPresent, "history for 'a' should be cleared when its override is removed")
	_, bPresent := s.histories["b"]
	assert.True(t, bPresent, "history for 'b' should be preserved (no override change)")
}

// --- runProbeCycle freshness gate ---

func TestRankCandidates_ExcludesEntriesBeforeCycleStart(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	recordSuccess(s, "a", 100)
	cycleStart := time.Now().Add(time.Second) // strictly after the entry
	ranked := s.rank(time.Now().Add(2*time.Second), cycleStart)
	assert.Empty(t, ranked, "entries before cycleStart must not appear in the ranked set")
}

// --- dataPlaneStream stall and idempotent close ---

type stallConn struct{ net.Conn }

func (stallConn) Read(p []byte) (int, error)       { return 0, errors.New("idle") }
func (stallConn) Write(p []byte) (int, error)      { return 0, errors.New("idle") }
func (stallConn) Close() error                     { return nil }
func (stallConn) LocalAddr() net.Addr              { return nil }
func (stallConn) RemoteAddr() net.Addr             { return nil }
func (stallConn) SetDeadline(time.Time) error      { return nil }
func (stallConn) SetReadDeadline(time.Time) error  { return nil }
func (stallConn) SetWriteDeadline(time.Time) error { return nil }

func TestDataPlaneStream_StallFiresOnce(t *testing.T) {
	var calls atomic.Uint32
	d := newDataPlaneStream(stallConn{}, 10*time.Millisecond, func() { calls.Add(1) }, nil, nil)
	defer d.Close()
	// Wait past the idle window with margin for scheduler jitter.
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		time.Second, 5*time.Millisecond, "stall should have fired by now")
	// Manually invoking fireStall a second time must be a no-op thanks
	// to the stalled CAS guard.
	d.fireStall()
	assert.Equal(t, uint32(1), calls.Load(), "stalled CAS should suppress duplicate fires")
}

func TestDataPlaneStream_CloseIsIdempotent(t *testing.T) {
	d := newDataPlaneStream(stallConn{}, time.Hour, func() {}, nil, nil)
	require.NoError(t, d.Close())
	assert.NoError(t, d.Close(), "second Close should be a no-op")
}

func TestDataPlaneStream_NoStallAfterClose(t *testing.T) {
	// Regression: Close sets stalled=true before stopping the timer, so a
	// racing Read/Write that fires the timer one more time after Close
	// (via a Reset that lost the race with Stop) lands on a no-op
	// fireStall. This test exercises the CAS guard directly: any fireStall
	// after Close must be suppressed.
	var calls atomic.Uint32
	d := newDataPlaneStream(stallConn{}, time.Hour, func() { calls.Add(1) }, nil, nil)
	d.Close()
	d.fireStall()
	assert.Equal(t, uint32(0), calls.Load(), "no stall callbacks must fire after Close")
}

func TestDataPlanePacket_NoStallAfterClose(t *testing.T) {
	var calls atomic.Uint32
	d := newDataPlanePacket(stallPacketConn{}, time.Hour, func() { calls.Add(1) }, nil, nil)
	d.Close()
	d.fireStall()
	assert.Equal(t, uint32(0), calls.Load(), "no stall callbacks must fire after Close")
}

type stallPacketConn struct{ net.PacketConn }

func (stallPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return 0, nil, errors.New("idle")
}
func (stallPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return 0, errors.New("idle") }
func (stallPacketConn) Close() error                              { return nil }
func (stallPacketConn) LocalAddr() net.Addr                       { return nil }
func (stallPacketConn) SetDeadline(time.Time) error               { return nil }
func (stallPacketConn) SetReadDeadline(time.Time) error           { return nil }
func (stallPacketConn) SetWriteDeadline(time.Time) error          { return nil }

// --- runLadder closed-group guard ---

func TestRunLadder_DoesNotEmitExhaustionWhenClosed(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.cancel() // mark closed before any ladder runs.
	s.runLadder("")
	select {
	case <-s.ExhaustionSignal():
		t.Errorf("closed group must not emit exhaustion")
	default:
	}
}

// --- persistence via AutoSelectHistoryStorage ---

func TestRecordOutcome_Persists(t *testing.T) {
	tests := []struct {
		name           string
		outcome        localOutcome
		delayMs        uint32
		wantOutcome    adapter.Outcome
		wantDelayMs    uint32
		wantConsecFail uint32
	}{
		{"success", outcomeSuccess, 123, adapter.OutcomeSuccess, 123, 0},
		{"failure also persisted, consec increments", outcomeTimeout, 0, adapter.OutcomeTimeout, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "a")
			s.recordOutcome("a", tt.outcome, tt.delayMs)
			got := s.history.Load("a")
			require.NotNil(t, got)
			require.Len(t, got.Outcomes, 1)
			assert.Equal(t, tt.wantOutcome, got.Outcomes[0].Outcome)
			assert.Equal(t, tt.wantDelayMs, got.Outcomes[0].DelayMs)
			assert.Equal(t, tt.wantConsecFail, got.ConsecutiveFailures)
		})
	}
}

func TestAutoSelectHistoryStorage_StoreAfterCloseDoesNotPanic(t *testing.T) {
	// Regression: in-flight probe/dataplane callbacks can outlive the
	// storage on tunnel shutdown. Late Store must drop quietly rather
	// than panic on a nil internal map.
	store := adapter.NewAutoSelectHistoryStorage()
	require.NoError(t, store.Close())
	assert.NotPanics(t, func() {
		store.Store("a", &adapter.TagHistory{UpdatedAt: time.Now()})
		store.Delete("a")
	}, "Store/Delete after Close must be safe no-ops")
	assert.Nil(t, store.Load("a"), "post-Close Store must not resurrect data")
}

func TestRecordOutcome_DropsOutcomesForRemovedTag(t *testing.T) {
	// Regression: a probe goroutine that started before Remove must not
	// resurrect the in-memory history entry or restore a deleted entry
	// in the persistence store.
	s, _ := newTestMUR(t, "a")
	mgr := &mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{
		"a": &mockOutbound{tag: "a"},
	}}
	s.outboundMgr = mgr

	// Pre-populate the persistence store, then Remove (which should clear it).
	s.recordOutcome("a", outcomeSuccess, 100)
	require.NotNil(t, s.history.Load("a"),
		"setup: expected persisted entry after success")
	_, err := s.Remove("a")
	require.NoError(t, err)
	require.Nil(t, s.history.Load("a"),
		"Remove must clear the persisted entry")

	// A late probe outcome must not re-create either layer.
	s.recordOutcome("a", outcomeSuccess, 200)
	s.access.Lock()
	_, exists := s.peekHistoryLocked("a")
	s.access.Unlock()
	assert.False(t, exists, "recordOutcome must not resurrect history for a non-member tag")
	assert.Nil(t, s.history.Load("a"),
		"recordOutcome must not resurrect the persisted entry")
}

// --- user-failure anti-laundering ---

func TestRecordUserFailure_BumpsCounterAndIsBoundedByMembership(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.recordUserFailure("a")
	s.recordUserFailure("a")
	s.access.Lock()
	h, ok := s.peekHistoryLocked("a")
	s.access.Unlock()
	require.True(t, ok)
	assert.Equal(t, uint32(2), h.userFailureCount())

	// A user-failure call for a non-member tag must not resurrect history.
	s.recordUserFailure("nonexistent")
	s.access.Lock()
	_, exists := s.peekHistoryLocked("nonexistent")
	s.access.Unlock()
	assert.False(t, exists, "recordUserFailure must not create history for a non-member tag")
}

func TestDataPlaneStream_UserFailureResetOnReadOnly(t *testing.T) {
	// Only Read should reset: a Write through a half-broken tunnel may
	// succeed locally (buffered) while remote receipt fails, so writes
	// must not count as bidirectional proof.
	tests := []struct {
		name      string
		op        func(d *dataPlaneStream) error
		wantCount uint32
	}{
		{
			name: "Read resets",
			op: func(d *dataPlaneStream) error {
				_, err := d.Read(make([]byte, 8))
				return err
			},
			wantCount: 0,
		},
		{
			name: "Write does not reset",
			op: func(d *dataPlaneStream) error {
				_, err := d.Write([]byte("hi"))
				return err
			},
			wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newLocalHistory(defaultHistoryRingMax)
			h.bumpUserFailures()
			d := newDataPlaneStream(echoConn{}, time.Hour, func() {}, nil, func() { h.resetUserFailures() })
			defer d.Close()
			require.NoError(t, tt.op(d))
			assert.Equal(t, tt.wantCount, h.userFailureCount())
		})
	}
}

func TestDemoted_UserFailuresTriggerEvenWithCleanProbes(t *testing.T) {
	// The whole point of the anti-laundering fix: a server can be
	// demoted by user-traffic failures alone, even if every probe
	// outcome in the ring is a success.
	now := time.Now()
	p := defaultHistoryParams()
	hist := []timestampedOutcome{
		{outcome: outcomeSuccess, timestamp: now.Add(-1 * time.Minute), delayMs: 100},
		{outcome: outcomeSuccess, timestamp: now.Add(-2 * time.Minute), delayMs: 110},
		{outcome: outcomeSuccess, timestamp: now.Add(-3 * time.Minute), delayMs: 120},
	}
	assert.False(t, demoted(hist, 0, 0, now, p),
		"healthy probe ring + no user failures: not demoted")
	assert.True(t, demoted(hist, 0, p.consecutiveFailLimit, now, p),
		"healthy probe ring but user-failure limit reached: demoted")
}

// --- selectFor: membership changes & seed-driven cold start ---

func TestSelectFor_PreservesStickyAfterUnrelatedRemove(t *testing.T) {
	// A Remove that doesn't touch the sticky tag must not flip
	// selection. (The bug this guards against was a routine server-list
	// update silently reverting a probe-driven pick back to "first
	// viable in tag order.")
	s, _ := newTestMUR(t, "a", "b", "c")
	recordSuccess(s, "a", 200)
	recordSuccess(s, "b", 150)
	recordSuccess(s, "c", 50)
	s.stickyTag.tcp.Store("c")
	if _, err := s.Remove("b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "c", got.Tag(),
		"selection should survive Remove of a non-sticky tag")
}

func TestSelectFor_RepicksWhenStickyGone(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c")
	recordSuccess(s, "a", 100)
	recordSuccess(s, "b", 50)
	recordSuccess(s, "c", 200)
	s.stickyTag.tcp.Store("b")
	if _, err := s.Remove("b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, []string{"a", "c"}, got.Tag(),
		"selection should be re-picked from remaining members")
	assert.NotEqual(t, "b", loadString(&s.stickyTag.tcp),
		"stickyTag should not still reference the removed member")
}

func TestSelectFor_PrefersLowestSeededDelay(t *testing.T) {
	// The cold-start initial pick should consult the
	// AutoSelectHistoryStorage seed via hydrate-on-Add so the offline
	// pre-warm informs the first selection — not just "first viable by
	// tag order."
	seed := func(delay uint32) *adapter.TagHistory {
		return &adapter.TagHistory{
			Outcomes: []adapter.TimestampedOutcome{{
				Outcome: adapter.OutcomeSuccess, DelayMs: delay, Timestamp: time.Now(),
			}},
			UpdatedAt: time.Now(),
		}
	}
	s, obs := newTestMUR(t, "a", "b", "c")
	s.history.Store("a", seed(500))
	s.history.Store("b", seed(300))
	s.history.Store("c", seed(100))
	// Hydrate in-memory history from storage (mimicking Start/Add).
	s.access.Lock()
	for _, tag := range s.tags {
		s.hydrateHistoryLocked(tag)
	}
	s.access.Unlock()

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "c", got.Tag(), "lowest seeded delay should win the cold-start pick")
	assert.Same(t, sbAdapter.Outbound(obs["c"]), got)
}

func TestSelectFor_FallsBackToTagOrderWithoutSeed(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c")
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "a", got.Tag(),
		"without seed data, the first viable tag should win the cold start (stable sort)")
}

func TestSelectFor_UnknownBeatsSubstitutedSeed(t *testing.T) {
	// Regression: every 3-minute config refresh removed the live
	// hysteria2 selection and added a fresh hysteria2 with no seed; the
	// previous recompute biased toward samizdat (substituteDelay
	// protocol with a real seed) over the unmeasured hysteria2, briefly
	// flipping the visible selection before the immediate probe cycle
	// switched back. The unified rank treats a kindUnknown candidate as
	// strictly better than a kindSubstituted one — probing the unknown
	// is preferable to committing to a deliberately deprioritized peer.
	s, obs := newTestMUR(t, "sami", "hysteria2-new")
	obs["sami"].typeName = lConst.TypeSamizdat
	s.history.Store("sami", &adapter.TagHistory{
		Outcomes: []adapter.TimestampedOutcome{{
			Outcome: adapter.OutcomeSuccess, DelayMs: 120, Timestamp: time.Now(),
		}},
		UpdatedAt: time.Now(),
	})
	// hysteria2-new has neither in-memory history nor a persisted seed.
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "hysteria2-new", got.Tag(),
		"unknown candidate must beat a substituted-delay seed")
}

// --- adaptive probe cadence ---

func TestNextProbeInterval(t *testing.T) {
	tests := []struct {
		name string
		prep func(s *MutableAutoSelect)
		want func(s *MutableAutoSelect) time.Duration
	}{
		{
			name: "no traffic observed defaults to idle",
			prep: func(*MutableAutoSelect) {},
			want: func(s *MutableAutoSelect) time.Duration { return s.cfg.idleInterval },
		},
		{
			name: "recent traffic selects active",
			prep: func(s *MutableAutoSelect) { s.bumpActive() },
			want: func(s *MutableAutoSelect) time.Duration { return s.cfg.activeInterval },
		},
		{
			name: "stale traffic backs off to idle",
			prep: func(s *MutableAutoSelect) {
				s.lastActive.Store(time.Now().Add(-2 * s.cfg.idleThreshold).UnixNano())
			},
			want: func(s *MutableAutoSelect) time.Duration { return s.cfg.idleInterval },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "a")
			tt.prep(s)
			assert.Equal(t, tt.want(s), s.nextProbeInterval())
		})
	}
}

func TestDataPlaneStream_OnActivityFiresOnNonEmptyIO(t *testing.T) {
	var activity atomic.Uint32
	d := newDataPlaneStream(echoConn{}, time.Hour, func() {}, func() { activity.Add(1) }, nil)
	defer d.Close()
	if _, err := d.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := d.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	assert.Equal(t, uint32(2), activity.Load(),
		"onActivity should fire once per non-empty Read/Write")
}

// echoConn returns n>0 on both Read and Write so the activity-tracking
// tests have a way to drive both call paths without setting up real I/O.
type echoConn struct{ net.Conn }

func (echoConn) Read(p []byte) (int, error)       { return len(p), nil }
func (echoConn) Write(p []byte) (int, error)      { return len(p), nil }
func (echoConn) Close() error                     { return nil }
func (echoConn) LocalAddr() net.Addr              { return nil }
func (echoConn) RemoteAddr() net.Addr             { return nil }
func (echoConn) SetDeadline(time.Time) error      { return nil }
func (echoConn) SetReadDeadline(time.Time) error  { return nil }
func (echoConn) SetWriteDeadline(time.Time) error { return nil }

func TestSetURLOverrides_ClearsPersistedEntryOnChange(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.recordOutcome("a", outcomeSuccess, 100)
	require.NotNil(t, s.history.Load("a"), "setup: expected persisted entry")
	s.SetURLOverrides(map[string]string{"a": "https://override.example/a"})
	assert.Nil(t, s.history.Load("a"),
		"override change must clear the persisted entry")
}

// --- recordUserFailure pushes a member into the soft-demote tier ---

func TestRecordUserFailure_DemotesTagInNextSelectFor(t *testing.T) {
	// After recordUserFailure("a"), "a" is in the soft tier and the
	// next selectFor("tcp") picks a clean peer instead — replaces the
	// old "synchronous clear of cached selection" contract.
	s, _ := newTestMUR(t, "a", "b")
	recordSuccess(s, "a", 50)
	recordSuccess(s, "b", 100)
	s.stickyTag.tcp.Store("a")

	s.recordUserFailure("a")

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "b", got.Tag(),
		"soft-demoted a must lose to clean b on the next selectFor")
}

// --- selectFor: soft-demote three-tier fallback ---

func TestSplitHealthyFor_PrefersCleanOverSoftOverHard(t *testing.T) {
	ob := &mockOutbound{networks: []string{"tcp", "udp"}}
	ranked := []rankedCandidate{
		{outbound: ob, tag: "hard", demote: demoteHard, delayMs: 10},
		{outbound: ob, tag: "soft", demote: demoteSoft, delayMs: 50},
		{outbound: ob, tag: "clean", demote: demoteClean, delayMs: 100},
	}
	pool := splitHealthyFor(ranked, "tcp")
	require.Len(t, pool, 1)
	assert.Equal(t, "clean", pool[0].tag, "clean dominates soft/hard regardless of delay")

	pool = splitHealthyFor(ranked[:2], "tcp")
	require.Len(t, pool, 1)
	assert.Equal(t, "soft", pool[0].tag, "soft preferred over hard when no clean exists")

	pool = splitHealthyFor(ranked[:1], "tcp")
	assert.Equal(t, 1, len(pool), "hard-only returns the hard slice as last resort")
}

// --- ladder skipStep1 at the first user failure ---

func TestRunLadder_SkipStep1(t *testing.T) {
	// Step 1 is skipped when the target has any user-failure evidence
	// (one is enough — well under the demote limit). Without that, the
	// ladder dials twice: step 1 fast retry plus step 2 re-probe.
	tests := []struct {
		name      string
		userFails int
		wantDials int
	}{
		{"no user failure: step 1 + step 2", 0, 2},
		{"one user failure: only step 2", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, obs := newTestMUR(t, "a")
			s.defaultURL = "http://probe.test/"
			obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))
			for i := 0; i < tt.userFails; i++ {
				s.access.Lock()
				s.historyForLocked("a").bumpUserFailures()
				s.access.Unlock()
			}
			s.runLadder("a")
			obs["a"].AssertNumberOfCalls(t, "DialContext", tt.wantDials)
		})
	}
}

// --- splitHealthyFor: per-network three-tier fallback ---

func TestSplitHealthyFor_KeepsSoftWhenCleanIsOtherNetwork(t *testing.T) {
	// The clean candidate is TCP-only; the only UDP-capable member is
	// soft. Per-network split must return the soft UDP member rather
	// than nil; the same pool can't be reused across networks.
	tcpOnly := &mockOutbound{tag: "tcp-only", networks: []string{"tcp"}}
	bothSoft := &mockOutbound{tag: "both-soft", networks: []string{"tcp", "udp"}}
	ranked := []rankedCandidate{
		{outbound: tcpOnly, tag: "tcp-only", demote: demoteClean, delayMs: 10},
		{outbound: bothSoft, tag: "both-soft", demote: demoteSoft, delayMs: 50},
	}
	tcpPool := splitHealthyFor(ranked, "tcp")
	require.Len(t, tcpPool, 1, "tcp pool should be clean-only")
	assert.Equal(t, "tcp-only", tcpPool[0].tag)

	udpPool := splitHealthyFor(ranked, "udp")
	require.Len(t, udpPool, 1, "udp pool should fall through to soft when no clean udp candidate exists")
	assert.Equal(t, "both-soft", udpPool[0].tag)
}

func TestSelectFor_PerNetworkPool(t *testing.T) {
	// Probe-cycle scenario: tcp-only is clean, both-soft is the only
	// UDP-capable member. Per-network pool selection means TCP gets
	// the clean tcp-only and UDP falls through to the soft both-soft.
	s, _ := newTestMUR(t, "tcp-only", "both-soft")
	tcpOnly := &mockOutbound{tag: "tcp-only", networks: []string{"tcp"}}
	bothSoft := &mockOutbound{tag: "both-soft", networks: []string{"tcp", "udp"}}
	s.members.Store("tcp-only", sbAdapter.Outbound(tcpOnly))
	s.members.Store("both-soft", sbAdapter.Outbound(bothSoft))
	recordSuccess(s, "tcp-only", 10)
	recordSuccess(s, "both-soft", 50)
	// Mark both-soft as soft-demoted.
	s.access.Lock()
	s.historyForLocked("both-soft").bumpUserFailures()
	s.access.Unlock()

	tcp, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, tcp)
	assert.Equal(t, "tcp-only", tcp.Tag(),
		"tcp must select the clean tcp-only member")

	udp, err := s.selectFor("udp")
	require.NoError(t, err)
	require.NotNil(t, udp)
	assert.Equal(t, "both-soft", udp.Tag(),
		"udp must fall through to the soft both-soft member when no clean udp candidate exists")
}

// --- selectFor kind tie-break ---

func TestSelectFor_PrefersRealSeededOverSubstituted(t *testing.T) {
	// Both "sami" (substituted via samizdat protocol) and "real" are
	// clean. s.tags order puts sami first, but selectFor should pick
	// real because real-seeded < substituted in kindOrder.
	s, obs := newTestMUR(t, "sami", "real")
	obs["sami"].typeName = lConst.TypeSamizdat
	recordSuccess(s, "real", 100)
	// "sami" needs a success too, otherwise selectFor sees it as
	// kindUnknown (which would also lose to real, but the test wants
	// to specifically pin the kindSubstituted-loses-to-kindRealSeeded
	// invariant).
	recordSuccess(s, "sami", 5)

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "real", got.Tag(),
		"real-seeded must win the kind tie-break over substituted even when substituted is first in s.tags")
}

func TestURLTest_HydratesBeforeRecordingOutcomes(t *testing.T) {
	// Seed the storage with a prior outcome ring for "a"; URLTest's
	// lazy-load must hydrate from this snapshot before recording a new
	// outcome, otherwise the persisted ring gets clobbered by a fresh
	// one-entry write.
	s, obs := newTestMUR(t, "a")
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))
	// Route URLTest's lazy-load through outboundMgr by dropping the
	// pre-populated member; otherwise the hydrate branch is skipped
	// because the member is already loaded.
	s.outboundMgr = &mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{
		"a": obs["a"],
	}}
	s.members.Delete("a")
	prior := time.Now().Add(-time.Minute)
	s.history.Store("a", &adapter.TagHistory{
		Outcomes: []adapter.TimestampedOutcome{
			{Outcome: adapter.OutcomeSuccess, DelayMs: 42, Timestamp: prior},
		},
		UpdatedAt: prior,
	})

	_, _ = s.URLTest(context.Background())

	got := s.history.Load("a")
	require.NotNil(t, got)
	require.GreaterOrEqual(t, len(got.Outcomes), 2,
		"hydrated entry must be appended to, not replaced — expected the seed plus the new failure")
	assert.Equal(t, adapter.OutcomeSuccess, got.Outcomes[0].Outcome,
		"the prior success seed must still be the first entry")
	assert.Equal(t, uint32(42), got.Outcomes[0].DelayMs)
}

// --- exhaustion signal end-to-end ---

// drainExhaustion empties any pending pre-existing signal so tests can
// observe the next emission deterministically.
func drainExhaustion(s *MutableAutoSelect) {
	select {
	case <-s.ExhaustionSignal():
	default:
	}
}

func TestExhaustionSignal_FiresOnFullLadderFailure(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b")
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))
	obs["b"].On("DialContext").Return(nil, errors.New("dial denied"))

	sig := s.ExhaustionSignal()
	s.runLadder("a")

	select {
	case _, ok := <-sig:
		assert.True(t, ok, "exhaustion channel should deliver a value, not be closed")
	case <-time.After(2 * time.Second):
		t.Fatal("exhaustion signal not delivered within timeout")
	}
}

func TestExhaustionSignal_CoalescesPendingSignal(t *testing.T) {
	// A stale pending signal must not block a fresh emission. emitExhaustion
	// drains any unread signal before sending, so the buffered channel always
	// reflects the latest ladder outcome.
	s, obs := newTestMUR(t, "a")
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))

	// Pre-load the buffer to simulate an unread prior signal.
	s.exhaustionCh <- struct{}{}
	require.Equal(t, 1, len(s.exhaustionCh), "setup: buffer should hold the stale signal")

	s.runLadder("a")

	select {
	case <-s.ExhaustionSignal():
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced exhaustion signal not delivered within timeout")
	}
	select {
	case <-s.ExhaustionSignal():
		t.Fatal("second receive should block: emitExhaustion buffers exactly one signal")
	default:
	}
}

func TestExhaustionSignal_CloseClosesChannelForRangeLoop(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range s.ExhaustionSignal() {
		}
	}()
	require.NoError(t, s.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("range loop did not exit after Close")
	}
}

// --- InterfaceUpdated triggers a probe cycle ---

func TestInterfaceUpdated_TriggersReprobe(t *testing.T) {
	s, obs := newTestMUR(t, "a")
	s.defaultURL = "http://probe.test/"
	dialed := make(chan struct{}, 1)
	obs["a"].On("DialContext").Run(func(mock.Arguments) {
		select {
		case dialed <- struct{}{}:
		default:
		}
	}).Return(nil, errors.New("dial denied"))

	s.InterfaceUpdated()

	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("InterfaceUpdated did not trigger a probe of member a")
	}
	// Wait for the probe cycle to finish so the goroutine's writes to the
	// mock's Calls log are happens-before our AssertNumberOfCalls read.
	s.probeMu.Lock()
	s.probeMu.Unlock()
	obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
}

// --- pause.Manager lookup in runBackgroundLoop ---

type fakePauseManager struct {
	pause.Manager
	registered atomic.Uint32
}

func (f *fakePauseManager) IsPaused() bool { return false }

func (f *fakePauseManager) RegisterCallback(pause.Callback) *list.Element[pause.Callback] {
	f.registered.Add(1)
	return &list.Element[pause.Callback]{}
}

func (f *fakePauseManager) UnregisterCallback(*list.Element[pause.Callback]) {}

func TestRunBackgroundLoop_RegistersWithPauseManager(t *testing.T) {
	pm := &fakePauseManager{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = service.ContextWith[pause.Manager](ctx, pm)
	s, _ := newTestMUR(t, "a")
	s.ctx = ctx

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runBackgroundLoop()
	}()
	require.Eventually(t, func() bool { return pm.registered.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "pause manager callback not registered")
	cancel()
	<-done
}

func TestRunBackgroundLoop_NoPauseManagerIsNoOp(t *testing.T) {
	// FromContext returns the zero value when no manager is registered;
	// the loop must skip the RegisterTicker branch without panicking.
	s, _ := newTestMUR(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runBackgroundLoop()
	}()
	// Give the loop a chance to enter its select; cancel then ensures clean exit.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBackgroundLoop did not exit after ctx cancel")
	}
}

// --- Close racing emitExhaustion under -race ---

func TestClose_RaceWithEmitExhaustionDoesNotPanic(t *testing.T) {
	// emitExhaustion sends on exhaustionCh and Close closes it; both
	// serialize through s.access so the send can never observe a closed
	// channel. This test stress-runs the two paths in parallel under the
	// race detector.
	const iterations = 200
	for i := 0; i < iterations; i++ {
		s, _ := newTestMUR(t, "a")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.emitExhaustion()
		}()
		go func() {
			defer wg.Done()
			_ = s.Close()
		}()
		wg.Wait()
	}
}
