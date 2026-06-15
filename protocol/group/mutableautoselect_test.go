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

// Type returns m.typeName so tests can drive behaviorFor branches
// (substituteDelay, excluded). Empty typeName keeps the default.
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
			ladderTotalBudget: 100 * time.Millisecond,
			dataPlaneIdle:     time.Hour,
			maxPersistedAge:   defaultMaxPersistedAge,
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

// recordSuccess writes a single fresh probe success to a member's
// history, returning the timestamp so callers can compute a cycleStart
// that includes (or excludes) the entry.
func recordSuccess(s *MutableAutoSelect, tag string, delay uint32) time.Time {
	now := time.Now()
	h := s.historyForLocked(tag)
	h.recordProbeSuccess(delay, now)
	return now
}

func TestPruneUserFailures(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		in     []time.Time
		window time.Duration
		want   int
	}{
		{"empty", nil, time.Minute, 0},
		{"all in window", []time.Time{now.Add(-1 * time.Minute), now.Add(-2 * time.Minute)}, 5 * time.Minute, 2},
		{"some aged out", []time.Time{now.Add(-1 * time.Minute), now.Add(-10 * time.Minute)}, 5 * time.Minute, 1},
		{"future-stamped clamps age to 0", []time.Time{now.Add(time.Hour)}, 5 * time.Minute, 1},
		{"all aged out returns nil", []time.Time{now.Add(-10 * time.Minute)}, 5 * time.Minute, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pruneUserFailures(append([]time.Time(nil), tt.in...), now, tt.window)
			assert.Len(t, got, tt.want)
		})
	}
}

func TestDemoted(t *testing.T) {
	p := defaultHistoryParams()
	tests := []struct {
		name      string
		consec    uint32
		userFails uint32
		wantHard  bool
		wantSoft  bool
	}{
		{"clean", 0, 0, false, false},
		{"consecutive at limit", p.consecutiveFailLimit, 0, true, false},
		{"consecutive below limit", p.consecutiveFailLimit - 1, 0, false, false},
		{"user failures at hard limit", 0, p.consecutiveFailLimit, true, false},
		{"user failures at soft limit", 0, p.softFailLimit, false, true},
		{"one user failure stays clean", 0, 1, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use selfMs=0 so the switch-penalty boost can't apply.
			hard, soft, _ := demoted(tt.consec, tt.userFails, 0, 0, p)
			assert.Equal(t, tt.wantHard, hard, "hard")
			assert.Equal(t, tt.wantSoft, soft, "soft")
		})
	}
}

func TestDemoted_SwitchPenaltyBoost(t *testing.T) {
	p := defaultHistoryParams()
	require.Equal(t, uint32(3), p.consecutiveFailLimit, "test assumes default limit of 3")

	// Without a faster self or a much slower alternative, the boost
	// doesn't apply and the rule trips at the base limit.
	hard, _, boosted := demoted(3, 0, 100, 150, p)
	assert.True(t, hard, "alt within latency factor must not boost")
	assert.False(t, boosted)

	// With a much slower alternative, the limit doubles. Three failures
	// no longer trip the rule; six do. consec-only failures don't flip
	// to soft (soft is reserved for user-traffic-side failures), so the
	// rescued state is fully clean.
	hard, soft, boosted := demoted(3, 0, 100, 990, p)
	assert.False(t, hard, "boosted limit must hold at 3 failures")
	assert.False(t, soft, "consec-only failures rescued by boost stay clean")
	assert.True(t, boosted, "boost flag must announce the rescue")

	hard, _, boosted = demoted(6, 0, 100, 990, p)
	assert.True(t, hard, "boosted limit must trip at 2x base")
	assert.True(t, boosted)

	// selfMs==0 (unknown delay) disables the boost.
	hard, _, boosted = demoted(3, 0, 0, 990, p)
	assert.True(t, hard, "unknown self-delay must not boost")
	assert.False(t, boosted)

	// User-failures path is also doubled.
	hard, _, boosted = demoted(0, 3, 100, 990, p)
	assert.False(t, hard, "user_failures path must also be boosted")
	assert.True(t, boosted)
	hard, _, _ = demoted(0, 6, 100, 990, p)
	assert.True(t, hard, "boosted user_failures must still trip at 2x base")
}

func TestLocalHistory_ConsecutiveFailuresResetOnSuccess(t *testing.T) {
	h := newLocalHistory()
	now := time.Now()
	h.recordProbeFailure(now)
	h.recordProbeFailure(now.Add(time.Second))
	_, _, consec, _ := h.snapshot(now, time.Hour)
	assert.Equal(t, uint32(2), consec, "two failures should accumulate")
	h.recordProbeSuccess(100, now.Add(2*time.Second))
	_, _, consec, _ = h.snapshot(now, time.Hour)
	assert.Equal(t, uint32(0), consec, "success should reset consecutive failures")
}

func TestLocalHistory_FailureDoesNotClearLastSuccessDelay(t *testing.T) {
	// A member with one transient failure must still have a real delay
	// measurement to rank by; the failure shows up in consecutive_failures
	// rather than by erasing the delay.
	h := newLocalHistory()
	now := time.Now()
	h.recordProbeSuccess(150, now)
	h.recordProbeFailure(now.Add(time.Second))
	lastDelay, _, consec, _ := h.snapshot(now, time.Hour)
	assert.Equal(t, uint32(150), lastDelay, "lastSuccessDelay must survive a subsequent failure")
	assert.Equal(t, uint32(1), consec, "consecutive failures bump")
}

func TestLocalHistory_UserFailuresIndependentOfProbeOutcomes(t *testing.T) {
	// A probe success must not clear user failures: probe and user
	// destinations live behind different classifiers, and a censor that
	// lets the probe through while dropping user traffic must not be
	// laundered to clean by a passing probe.
	h := newLocalHistory()
	now := time.Now()
	h.addUserFailure(now, 5*time.Minute, 0)
	h.addUserFailure(now.Add(time.Second), 5*time.Minute, 0)
	assert.Equal(t, uint32(2), h.userFailureCount(now.Add(2*time.Second), 5*time.Minute))
	h.recordProbeSuccess(100, now.Add(3*time.Second))
	assert.Equal(t, uint32(2), h.userFailureCount(now.Add(4*time.Second), 5*time.Minute),
		"probe success must NOT clear the user-failure window")
}

func TestLocalHistory_UserFailuresAgeOut(t *testing.T) {
	// Failures age out of the window naturally — no explicit reset
	// path. A member that recovers self-clears in one userFailureWindow
	// regardless of whether traffic gets routed through it.
	h := newLocalHistory()
	now := time.Now()
	h.addUserFailure(now, 5*time.Minute, 0)
	h.addUserFailure(now.Add(time.Minute), 5*time.Minute, 0)
	assert.Equal(t, uint32(2), h.userFailureCount(now.Add(time.Minute), 5*time.Minute))
	// Six minutes later, both are stale.
	assert.Equal(t, uint32(0), h.userFailureCount(now.Add(6*time.Minute), 5*time.Minute),
		"failures must age out of the window without an explicit reset")
}

func TestLocalHistory_AddUserFailureDedupesBurst(t *testing.T) {
	// A single broken outbound with many orphaned conns hitting their
	// stall timer in sequence would otherwise inflate the count out of
	// proportion to the event. The dedupe window collapses bursts to
	// one failure.
	h := newLocalHistory()
	now := time.Now()
	dedupe := 30 * time.Second

	require.True(t, h.addUserFailure(now, 5*time.Minute, dedupe), "first failure must record")
	require.False(t, h.addUserFailure(now.Add(time.Second), 5*time.Minute, dedupe), "burst within dedupe must drop")
	require.False(t, h.addUserFailure(now.Add(29*time.Second), 5*time.Minute, dedupe), "still within dedupe must drop")
	assert.Equal(t, uint32(1), h.userFailureCount(now.Add(30*time.Second), 5*time.Minute), "burst collapsed to one")

	require.True(t, h.addUserFailure(now.Add(30*time.Second), 5*time.Minute, dedupe), "at-or-past dedupe must record")
	assert.Equal(t, uint32(2), h.userFailureCount(now.Add(30*time.Second), 5*time.Minute))
}

func TestHydrateLocalHistory_DropsAgedUserFailures(t *testing.T) {
	now := time.Now()
	persisted := &adapter.TagHistory{
		LastSuccessDelayMs: 120,
		LastOutcomeAt:      now.Add(-time.Minute),
		UserFailures: []time.Time{
			now.Add(-30 * time.Minute), // outside 5-min window
			now.Add(-1 * time.Minute),  // inside window
		},
		UpdatedAt: now.Add(-time.Minute),
	}
	h := hydrateLocalHistory(persisted, now, 5*time.Minute)
	lastDelay, _, _, userFails := h.snapshot(now, 5*time.Minute)
	assert.Equal(t, uint32(120), lastDelay)
	assert.Len(t, userFails, 1, "stale user-failure timestamp must be dropped on hydrate")
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

func TestRank_SwitchPenaltyOnlyAppliesToRealSeeded(t *testing.T) {
	// A kindUnknown candidate (no probe yet) must never be boosted —
	// the boost compares its delay against its peers' best, but its
	// own delay is 0 (sentinel for "no data"), not a real measurement.
	// Without the kindRealSeeded gate, the limit would double off a
	// synthetic 0-vs-real comparison.
	s, _ := newTestMUR(t, "fast-real", "unknown-mid", "slow-real")
	recordSuccess(s, "fast-real", 100) // 100ms — fastest, would be boosted
	recordSuccess(s, "slow-real", 990) // 990ms — the much-slower alternative
	// "unknown-mid" intentionally has no probe success — kindUnknown.

	// Three user-failures on each — enough to trip the base limit.
	addUserFailureN(s, "fast-real", int(s.hist.consecutiveFailLimit))
	addUserFailureN(s, "unknown-mid", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now(), time.Time{})
	s.access.Unlock()

	var fast, unknown rankedCandidate
	for _, c := range ranked {
		switch c.tag {
		case "fast-real":
			fast = c
		case "unknown-mid":
			unknown = c
		}
	}
	require.NotEmpty(t, fast.tag, "fast-real must appear in rank")
	require.NotEmpty(t, unknown.tag, "unknown-mid must appear in rank")

	assert.Equal(t, demoteSoft, fast.demote,
		"fast-real should be rescued from hard-demote (best alt 990ms is >3x its 100ms)")
	assert.Equal(t, demoteHard, unknown.demote,
		"unknown-mid must not be rescued — its 0ms self-delay is a sentinel, not a measurement")
}

// addUserFailureN appends n failures to tag's in-memory history.
// Passes dedupe=0 so the helper can deterministically seed any count
// without depending on the dedupe window.
func addUserFailureN(s *MutableAutoSelect, tag string, n int) {
	s.access.Lock()
	defer s.access.Unlock()
	h := s.historyForLocked(tag)
	now := time.Now()
	for i := 0; i < n; i++ {
		h.addUserFailure(now.Add(time.Duration(i)*time.Millisecond), s.hist.userFailureWindow, 0)
	}
}

func TestSelectFor_ThreeTierFallback(t *testing.T) {
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
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
			},
			wantTag: "b",
		},
		{
			name: "soft-only pool returns the soft member",
			setup: func(s *MutableAutoSelect) {
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
				addUserFailureN(s, "b", int(s.hist.softFailLimit))
			},
			wantTag: "a",
		},
		{
			name: "all-hard falls through to lowest-delay hard",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				recordSuccess(s, "b", 200)
				addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
				addUserFailureN(s, "b", int(s.hist.consecutiveFailLimit))
			},
			wantTag: "a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "a", "b")
			tt.setup(s)
			got, err := s.selectFor("tcp")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantTag, got.Tag())
		})
	}
}

func TestSelectFor_SwitchTolerance(t *testing.T) {
	tests := []struct {
		name       string
		alphaDelay uint32
		betaDelay  uint32
		want       string
		commentary string
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
	s, _ := newTestMUR(t, "alpha", "beta")
	recordSuccess(s, "beta", 120)
	addUserFailureN(s, "alpha", int(s.hist.consecutiveFailLimit))
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
	assert.Equal(t, "a", loadString(&s.stickyTag.tcp))
}

func TestSelectForExcluding_SkipsTargetAndDoesNotMutateSticky(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	recordSuccess(s, "a", 30)
	recordSuccess(s, "b", 50)
	s.stickyTag.tcp.Store("a")

	got, err := s.selectForExcluding("tcp", "a")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "b", got.Tag(), "fast-failover must skip the excluded tag")
	assert.Equal(t, "a", loadString(&s.stickyTag.tcp),
		"a fast-failover pick must not poison the sticky tag")
}

func TestSelectForExcluding_NoAlternateReturnsError(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	recordSuccess(s, "a", 50)
	got, err := s.selectForExcluding("tcp", "a")
	assert.Error(t, err)
	assert.Nil(t, got)
}

func TestPeekHistoryLocked_DoesNotCreateEntry(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	_, ok := s.peekHistoryLocked("a")
	assert.False(t, ok)
	_, present := s.histories["a"]
	assert.False(t, present, "peek leaked an empty history entry into the map")
}

func TestRemove_PreservesTagOrder(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c", "d")
	n, err := s.Remove("b")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, []string{"a", "c", "d"}, s.All())
}

func TestAdd_DoesNotDuplicateAlreadyListedTag(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	mgr := &mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{
		"a": &mockOutbound{tag: "a"},
	}}
	s.outboundMgr = mgr
	s.members.Delete("a")
	n, err := s.Add("a")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, []string{"a"}, s.All())
}

func TestSetURLOverrides_RemovingOverrideInvalidatesHistory(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	s.urlOverrides = map[string]string{"a": "https://override.example/a"}
	recordSuccess(s, "a", 100)
	recordSuccess(s, "b", 200)

	s.SetURLOverrides(map[string]string{})

	_, aPresent := s.histories["a"]
	assert.False(t, aPresent, "history for 'a' should be cleared when its override is removed")
	_, bPresent := s.histories["b"]
	assert.True(t, bPresent, "history for 'b' should be preserved (no override change)")
}

func TestRank_ExcludesEntriesBeforeCycleStart(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	recordSuccess(s, "a", 100)
	cycleStart := time.Now().Add(time.Second)
	s.access.Lock()
	ranked := s.rankLocked(time.Now().Add(2*time.Second), cycleStart)
	s.access.Unlock()
	assert.Empty(t, ranked, "entries before cycleStart must not appear in the ranked set")
}

type stallConn struct{ net.Conn }

func (stallConn) Read(p []byte) (int, error)       { return 0, errors.New("idle") }
func (stallConn) Write(p []byte) (int, error)      { return 0, errors.New("idle") }
func (stallConn) Close() error                     { return nil }
func (stallConn) LocalAddr() net.Addr              { return nil }
func (stallConn) RemoteAddr() net.Addr             { return nil }
func (stallConn) SetDeadline(time.Time) error      { return nil }
func (stallConn) SetReadDeadline(time.Time) error  { return nil }
func (stallConn) SetWriteDeadline(time.Time) error { return nil }

func TestDataPlaneStream_StallSuppressedUntilProven(t *testing.T) {
	// A handshake-only conn that never delivers payload is "established
	// but inactive" — the stall timer fires but onStall is suppressed
	// because provedReadBytes hasn't been crossed.
	var calls atomic.Uint32
	const provedReadBytes = 100
	d := newDataPlaneStream(stallConn{}, 10*time.Millisecond, provedReadBytes,
		func() { calls.Add(1) }, nil)
	defer d.Close()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, uint32(0), calls.Load(),
		"unproven conn going idle is not a stall")
}

func TestDataPlaneStream_StallFiresAfterProven(t *testing.T) {
	// Once cumulative Read bytes cross provedReadBytes AND the most recent
	// IO was a Write that hasn't been answered, the next idle window
	// fires onStall exactly once.
	var calls atomic.Uint32
	const provedReadBytes = 50
	d := newDataPlaneStream(echoConn{}, 30*time.Millisecond, provedReadBytes,
		func() { calls.Add(1) }, nil)
	defer d.Close()
	// Drive enough Read bytes to prove the conn.
	if _, err := d.Read(make([]byte, provedReadBytes)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	require.True(t, d.proven.Load(), "Read should have proven the conn")
	// Drive a Write so the watchdog sees "we sent bytes, no response."
	if _, err := d.Write([]byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	require.True(t, d.lastWasWrite.Load(), "Write should set the last-was-write gate")
	// Wait past the idle window.
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		time.Second, 5*time.Millisecond, "proven stall should have fired by now")

	d.fireStall()
	assert.Equal(t, uint32(1), calls.Load(), "fired CAS should suppress duplicate fires")
}

func TestDataPlaneStream_StallSuppressedOnReadOnlyIdle(t *testing.T) {
	// A proven conn whose last IO was a Read is user-idle, not broken:
	// the response arrived and the user stopped sending. Suppress the
	// stall to avoid demoting a healthy outbound on a quiet HTTPS
	// keep-alive.
	var calls atomic.Uint32
	const provedReadBytes = 50
	d := newDataPlaneStream(echoConn{}, 30*time.Millisecond, provedReadBytes,
		func() { calls.Add(1) }, nil)
	defer d.Close()
	if _, err := d.Read(make([]byte, provedReadBytes)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	require.True(t, d.proven.Load(), "Read should have proven the conn")
	require.False(t, d.lastWasWrite.Load(), "Read-only conn must not set the last-was-write gate")
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, uint32(0), calls.Load(),
		"proven conn with read-only history must not fire stall")
}

func TestDataPlaneStream_CloseIsIdempotent(t *testing.T) {
	d := newDataPlaneStream(stallConn{}, time.Hour, 0, func() {}, nil)
	require.NoError(t, d.Close())
	assert.NoError(t, d.Close(), "second Close should be a no-op")
}

func TestDataPlaneStream_NoStallAfterClose(t *testing.T) {
	var calls atomic.Uint32
	d := newDataPlaneStream(stallConn{}, time.Hour, 0, func() { calls.Add(1) }, nil)
	d.proven.Store(true)       // simulate a proven conn so fireStall isn't gated on the proven check
	d.lastWasWrite.Store(true) // ...nor on the write-without-read gate
	d.Close()
	d.fireStall()
	assert.Equal(t, uint32(0), calls.Load(), "no stall callbacks must fire after Close")
}

func TestDataPlanePacket_NoStallAfterClose(t *testing.T) {
	var calls atomic.Uint32
	d := newDataPlanePacket(stallPacketConn{}, time.Hour, 0, func() { calls.Add(1) }, nil)
	d.proven.Store(true)
	d.lastWasWrite.Store(true)
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

func TestRunLadder_DoesNotEmitExhaustionWhenClosed(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.cancel()
	s.runLadder("")
	select {
	case <-s.ExhaustionSignal():
		t.Errorf("closed group must not emit exhaustion")
	default:
	}
}

func TestRecordProbeOutcome_Persists(t *testing.T) {
	tests := []struct {
		name        string
		success     bool
		delayMs     uint32
		wantDelayMs uint32
		wantConsec  uint32
	}{
		{"success records delay and zeroes consecutive", true, 123, 123, 0},
		{"failure increments consecutive, preserves delay", false, 0, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestMUR(t, "a")
			s.recordProbeOutcome("a", tt.success, tt.delayMs)
			got := s.history.Load("a")
			require.NotNil(t, got)
			assert.Equal(t, tt.wantDelayMs, got.LastSuccessDelayMs)
			assert.Equal(t, tt.wantConsec, got.ConsecutiveFailures)
		})
	}
}

func TestAutoSelectHistoryStorage_StoreAfterCloseDoesNotPanic(t *testing.T) {
	store := adapter.NewAutoSelectHistoryStorage()
	require.NoError(t, store.Close())
	assert.NotPanics(t, func() {
		store.Store("a", &adapter.TagHistory{UpdatedAt: time.Now()})
		store.Delete("a")
	})
	assert.Nil(t, store.Load("a"))
}

func TestRecordProbeOutcome_DropsOutcomesForRemovedTag(t *testing.T) {
	// A probe goroutine that started before Remove must not resurrect
	// the in-memory history entry or restore a deleted entry in the
	// persistence store.
	s, _ := newTestMUR(t, "a")
	mgr := &mockOutboundManager{outbounds: map[string]sbAdapter.Outbound{
		"a": &mockOutbound{tag: "a"},
	}}
	s.outboundMgr = mgr

	s.recordProbeOutcome("a", true, 100)
	require.NotNil(t, s.history.Load("a"))
	_, err := s.Remove("a")
	require.NoError(t, err)
	require.Nil(t, s.history.Load("a"))

	s.recordProbeOutcome("a", true, 200)
	s.access.Lock()
	_, exists := s.peekHistoryLocked("a")
	s.access.Unlock()
	assert.False(t, exists, "recordProbeOutcome must not resurrect history for a non-member tag")
	assert.Nil(t, s.history.Load("a"))
}

func TestRecordUserFailure_AppendsAndBoundedByMembership(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	assert.True(t, s.recordUserFailure("a"), "first call must persist")
	assert.False(t, s.recordUserFailure("a"), "immediate burst must return false (deduped)")
	s.access.Lock()
	h, ok := s.peekHistoryLocked("a")
	s.access.Unlock()
	require.True(t, ok)
	assert.Equal(t, uint32(1), h.userFailureCount(time.Now(), s.hist.userFailureWindow),
		"successive calls within dedupe window must collapse")

	assert.False(t, s.recordUserFailure("nonexistent"), "non-member must return false")
	s.access.Lock()
	_, exists := s.peekHistoryLocked("nonexistent")
	s.access.Unlock()
	assert.False(t, exists)
}

func TestMakeHooks_StallAppendsSingleUserFailure(t *testing.T) {
	// A stall counts as one failure — same weight as a single dial
	// error. The demote rule hits hard at three failures in window, not
	// one. (Spec change from earlier "one-shot hard demote.")
	s, _ := newTestMUR(t, "a")
	onStall, _ := s.makeHooks("a")
	onStall()
	s.access.Lock()
	h, ok := s.peekHistoryLocked("a")
	s.access.Unlock()
	require.True(t, ok)
	assert.Equal(t, uint32(1), h.userFailureCount(time.Now(), s.hist.userFailureWindow),
		"a single stall must contribute exactly one user-failure timestamp")
}

func TestRecordUserFailure_DemotesTagInNextSelectFor(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b")
	recordSuccess(s, "a", 50)
	recordSuccess(s, "b", 100)
	s.stickyTag.tcp.Store("a")

	// addUserFailureN bypasses the dedupe window, so the soft-limit
	// failures land deterministically without depending on wall time.
	addUserFailureN(s, "a", int(s.hist.softFailLimit))

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "b", got.Tag(),
		"soft-demoted a must lose to clean b on the next selectFor")
}

func TestSplitHealthyFor_PrefersCleanOverSoftOverHard(t *testing.T) {
	ob := &mockOutbound{networks: []string{"tcp", "udp"}}
	mk := func(tag string, d demoteLevel) rankedCandidate {
		return rankedCandidate{outbound: ob, tag: tag, demote: d}
	}

	s := &MutableAutoSelect{}
	// ranked must be demote-sorted (clean < soft < hard), as rankLocked returns it.
	pool := s.splitHealthyForLocked([]rankedCandidate{mk("clean", demoteClean), mk("soft", demoteSoft), mk("hard", demoteHard)}, "tcp")
	require.Len(t, pool, 1)
	assert.Equal(t, "clean", pool[0].tag)

	pool = s.splitHealthyForLocked([]rankedCandidate{mk("soft", demoteSoft), mk("hard", demoteHard)}, "tcp")
	require.Len(t, pool, 1)
	assert.Equal(t, "soft", pool[0].tag)

	pool = s.splitHealthyForLocked([]rankedCandidate{mk("hard", demoteHard)}, "tcp")
	assert.Len(t, pool, 1)
}

func TestSplitHealthyFor_KeepsSoftWhenCleanIsOtherNetwork(t *testing.T) {
	tcpOnly := &mockOutbound{tag: "tcp-only", networks: []string{"tcp"}}
	bothSoft := &mockOutbound{tag: "both-soft", networks: []string{"tcp", "udp"}}
	ranked := []rankedCandidate{
		{outbound: tcpOnly, tag: "tcp-only", demote: demoteClean, delayMs: 10},
		{outbound: bothSoft, tag: "both-soft", demote: demoteSoft, delayMs: 50},
	}
	s := &MutableAutoSelect{}
	tcpPool := s.splitHealthyForLocked(ranked, "tcp")
	require.Len(t, tcpPool, 1, "tcp pool should be clean-only")
	assert.Equal(t, "tcp-only", tcpPool[0].tag)

	udpPool := s.splitHealthyForLocked(ranked, "udp")
	require.Len(t, udpPool, 1, "udp pool should fall through to soft when no clean udp candidate exists")
	assert.Equal(t, "both-soft", udpPool[0].tag)
}

func TestSelectFor_PerNetworkPool(t *testing.T) {
	s, _ := newTestMUR(t, "tcp-only", "both-soft")
	tcpOnly := &mockOutbound{tag: "tcp-only", networks: []string{"tcp"}}
	bothSoft := &mockOutbound{tag: "both-soft", networks: []string{"tcp", "udp"}}
	s.members.Store("tcp-only", sbAdapter.Outbound(tcpOnly))
	s.members.Store("both-soft", sbAdapter.Outbound(bothSoft))
	recordSuccess(s, "tcp-only", 10)
	recordSuccess(s, "both-soft", 50)
	addUserFailureN(s, "both-soft", int(s.hist.softFailLimit))

	tcp, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, tcp)
	assert.Equal(t, "tcp-only", tcp.Tag())

	udp, err := s.selectFor("udp")
	require.NoError(t, err)
	require.NotNil(t, udp)
	assert.Equal(t, "both-soft", udp.Tag())
}

func TestSelectFor_PreservesStickyAfterUnrelatedRemove(t *testing.T) {
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
	assert.Equal(t, "c", got.Tag())
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
	assert.Contains(t, []string{"a", "c"}, got.Tag())
	assert.NotEqual(t, "b", loadString(&s.stickyTag.tcp))
}

func TestSelectFor_PrefersLowestSeededDelay(t *testing.T) {
	seed := func(delay uint32) *adapter.TagHistory {
		return &adapter.TagHistory{
			LastSuccessDelayMs: delay,
			LastOutcomeAt:      time.Now(),
			UpdatedAt:          time.Now(),
		}
	}
	s, obs := newTestMUR(t, "a", "b", "c")
	s.history.Store("a", seed(500))
	s.history.Store("b", seed(300))
	s.history.Store("c", seed(100))
	s.access.Lock()
	for _, tag := range s.tags {
		s.hydrateHistoryLocked(tag)
	}
	s.access.Unlock()

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "c", got.Tag())
	assert.Same(t, sbAdapter.Outbound(obs["c"]), got)
}

func TestSelectFor_FallsBackToTagOrderWithoutSeed(t *testing.T) {
	s, _ := newTestMUR(t, "a", "b", "c")
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "a", got.Tag())
}

func TestHydrate_DropsStaleSnapshot(t *testing.T) {
	// Persisted snapshots older than maxPersistedAge are dropped on
	// hydrate — a bandit-driven candidate set typically rotates every
	// 60 s – 15 min, so an hour-old entry usually describes a member
	// that no longer exists.
	s, _ := newTestMUR(t, "a")
	stale := &adapter.TagHistory{
		LastSuccessDelayMs: 100,
		UpdatedAt:          time.Now().Add(-2 * defaultMaxPersistedAge),
	}
	s.history.Store("a", stale)
	s.access.Lock()
	delete(s.histories, "a")
	s.hydrateHistoryLocked("a")
	_, ok := s.histories["a"]
	s.access.Unlock()
	assert.False(t, ok, "stale snapshot must not hydrate")
	assert.Nil(t, s.history.Load("a"), "stale snapshot must be deleted on hydrate too")
}

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
	d := newDataPlaneStream(echoConn{}, time.Hour, 0, func() {}, func() { activity.Add(1) })
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
	s.recordProbeOutcome("a", true, 100)
	require.NotNil(t, s.history.Load("a"))
	s.SetURLOverrides(map[string]string{"a": "https://override.example/a"})
	assert.Nil(t, s.history.Load("a"))
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
	s, obs := newTestMUR(t, "a")
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))

	s.exhaustionCh <- struct{}{}
	require.Equal(t, 1, len(s.exhaustionCh))

	s.runLadder("a")

	select {
	case <-s.ExhaustionSignal():
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced exhaustion signal not delivered within timeout")
	}
	select {
	case <-s.ExhaustionSignal():
		t.Fatal("second receive should block")
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

func TestRunLadder_NoStep1Retry(t *testing.T) {
	// The ladder is just a full re-probe — there is no "fast retry of
	// the failing member" anymore. After one ladder run, the failing
	// member is dialed exactly once (the probe cycle's single dial),
	// not twice.
	s, obs := newTestMUR(t, "a")
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))

	s.runLadder("a")
	obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
}

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
	s.probeMu.Lock()
	s.probeMu.Unlock()
	obs["a"].AssertNumberOfCalls(t, "DialContext", 1)
}

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
		2*time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

func TestRunBackgroundLoop_NoPauseManagerIsNoOp(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runBackgroundLoop()
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBackgroundLoop did not exit after ctx cancel")
	}
}

func TestClose_RaceWithEmitExhaustionDoesNotPanic(t *testing.T) {
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
