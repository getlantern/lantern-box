package group

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lConst "github.com/getlantern/lantern-box/constant"
)

// newLastResortMUR builds a group with a regular member "a" and an unbounded
// member "ub".
func newLastResortMUR(t *testing.T) (*MutableAutoSelect, map[string]*mockOutbound) {
	t.Helper()
	s, obs := newTestMUR(t, "a", "ub")
	obs["ub"].typeName = lConst.TypeUnbounded
	return s, obs
}

func TestSelectFor_LastResortOnlyWhenNothingHealthy(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(s *MutableAutoSelect)
		wantTag string
	}{
		{
			name: "clean regular member beats a faster last resort",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 900)
				recordSuccess(s, "ub", 50)
			},
			wantTag: "a",
		},
		{
			name: "soft-demoted regular member still beats the last resort",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 900)
				recordSuccess(s, "ub", 50)
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
			},
			wantTag: "a",
		},
		{
			name: "last resort carries traffic once every regular member is hard-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				recordSuccess(s, "ub", 5000)
				addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
			},
			wantTag: "ub",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newLastResortMUR(t)
			tt.setup(s)
			got, err := s.selectFor("tcp")
			require.NoError(t, err)
			assert.Equal(t, tt.wantTag, got.Tag())
		})
	}
}

func TestRank_LastResortTierSitsBetweenSoftAndHard(t *testing.T) {
	s, obs := newTestMUR(t, "soft", "ub", "hard")
	obs["ub"].typeName = lConst.TypeUnbounded
	recordSuccess(s, "soft", 100)
	recordSuccess(s, "ub", 100)
	recordSuccess(s, "hard", 100)
	addUserFailureN(s, "soft", int(s.hist.softFailLimit))
	addUserFailureN(s, "hard", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now(), time.Time{})
	s.access.Unlock()

	var order []string
	levels := map[string]demoteLevel{}
	for _, c := range ranked {
		order = append(order, c.tag)
		levels[c.tag] = c.demote
	}
	assert.Equal(t, []string{"soft", "ub", "hard"}, order)
	assert.Equal(t, demoteLastResort, levels["ub"])
}

func TestRank_HardDemotedLastResortStaysHard(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now(), time.Time{})
	s.access.Unlock()
	for _, c := range ranked {
		if c.tag == "ub" {
			assert.Equal(t, demoteHard, c.demote, "a failing last resort must not outrank failing regular members")
		}
	}
}

func TestSelectFor_LeavesLastResortWhenARegularMemberRecovers(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 100)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.Equal(t, "ub", got.Tag())

	// "a" recovers, and is slower than the sticky last resort by more than
	// the switch tolerance would normally require to move.
	s.access.Lock()
	delete(s.histories, "a")
	s.access.Unlock()
	recordSuccess(s, "a", 100)

	got, err = s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag())
}

func TestCollectProbeJobs_SkipsLastResort(t *testing.T) {
	s, _ := newLastResortMUR(t)
	s.access.Lock()
	defer s.access.Unlock()
	assert.Equal(t, []string{"a"}, jobTags(s.collectProbeJobsLocked(time.Now(), nil, true)))
}

// TestInternalProbe_DoesNotWaitForLastResort checks a slow last-resort
// handshake can't hold up a probe wave, and that its outcome still lands
// in history once it completes.
func TestInternalProbe_DoesNotWaitForLastResort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)

	s, obs := newLastResortMUR(t)
	s.defaultURL = "http://probe.test/"
	obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))
	release := make(chan struct{})
	var dials atomic.Int32
	obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}

	done := make(chan struct{})
	go func() {
		s.probeMu.Lock()
		s.internalProbe(context.Background(), nil)
		s.probeMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "probe wave waited on the last-resort member")
	}

	// A second wave while the first last-resort probe is still running
	// must not start another.
	s.probeMu.Lock()
	s.internalProbe(context.Background(), nil)
	s.probeMu.Unlock()

	close(release)
	require.Eventually(t, func() bool {
		s.access.Lock()
		defer s.access.Unlock()
		h, ok := s.peekHistoryLocked("ub")
		if !ok {
			return false
		}
		delay, _, consec, _, _ := h.snapshot(time.Now(), s.hist.userFailureWindow)
		return delay > 0 && consec == 0
	}, 5*time.Second, 10*time.Millisecond, "last-resort probe outcome was not recorded")
	assert.Equal(t, int32(1), dials.Load(), "a last-resort member has at most one probe in flight")
}

func TestLastResortProbe_DropsOutcomeOnShutdown(t *testing.T) {
	s, obs := newLastResortMUR(t)
	s.defaultURL = "http://probe.test/"
	started := make(chan struct{})
	obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), nil, true)
	s.access.Unlock()
	<-started
	s.cancel()

	require.Eventually(t, func() bool {
		s.access.Lock()
		defer s.access.Unlock()
		return !s.lastResortInFlight["ub"]
	}, 2*time.Second, 10*time.Millisecond)
	s.access.Lock()
	_, ok := s.peekHistoryLocked("ub")
	s.access.Unlock()
	assert.False(t, ok, "shutdown is not evidence against the member")
}

func TestRunLadder_LastResortWithLatestSuccessIsNotExhausted(t *testing.T) {
	tests := []struct {
		name           string
		latestFailed   bool
		successAge     time.Duration
		wantExhaustion bool
	}{
		{"latest last-resort probe succeeded", false, 0, false},
		{"latest last-resort probe failed", true, 0, true},
		{"latest success is stale", false, lastResortSuccessFreshness + time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, obs := newLastResortMUR(t)
			s.defaultURL = "http://probe.test/"
			obs["a"].On("DialContext").Return(nil, errors.New("dial denied"))
			// The ladder kicks a last-resort probe; keep it pending so it
			// can't change the outcome under test.
			obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if tt.successAge > 0 {
				s.access.Lock()
				s.historyForLocked("ub").recordProbeSuccess(4000, time.Now().Add(-tt.successAge))
				s.access.Unlock()
			} else {
				recordSuccess(s, "ub", 4000)
			}
			if tt.latestFailed {
				s.recordProbeOutcome("ub", false, 0)
			}

			sig := s.ExhaustionSignal()
			s.runLadder("a")

			select {
			case <-sig:
				assert.True(t, tt.wantExhaustion, "a last resort whose latest probe succeeded can still carry traffic")
			default:
				assert.False(t, tt.wantExhaustion, "a last resort whose latest probe failed is no winner")
			}
		})
	}
}

// TestLastResortProbe_WatchdogFailsOverrunningProbe covers an outbound
// whose dial overruns its deadline: the probe is recorded as failed at the
// watchdog, but the slot stays taken until the dial returns, so a stalled
// peer can't accumulate stuck dials across cycles.
func TestLastResortProbe_WatchdogFailsOverrunningProbe(t *testing.T) {
	orig := lastResortProbeBound
	lastResortProbeBound = func(protocolBehavior) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { lastResortProbeBound = orig })

	s, obs := newLastResortMUR(t)
	s.defaultURL = "http://probe.test/"
	stuck := make(chan struct{})
	var dials atomic.Int32
	obs["ub"].dial = func(context.Context) (net.Conn, error) {
		dials.Add(1)
		<-stuck
		return nil, errors.New("released")
	}
	kick := func() {
		s.access.Lock()
		s.kickLastResortProbesLocked(time.Now(), nil, true)
		s.access.Unlock()
	}

	kick()
	require.Eventually(t, func() bool {
		s.access.Lock()
		defer s.access.Unlock()
		h, ok := s.peekHistoryLocked("ub")
		if !ok {
			return false
		}
		_, _, consec, _, _ := h.snapshot(time.Now(), s.hist.userFailureWindow)
		return consec == 1
	}, 2*time.Second, 10*time.Millisecond, "an overrunning probe must be recorded as failed at the watchdog")

	kick()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), dials.Load(), "no second dial while the first is still stuck")

	close(stuck)
	require.Eventually(t, func() bool {
		s.access.Lock()
		defer s.access.Unlock()
		return !s.lastResortInFlight["ub"]
	}, 2*time.Second, 10*time.Millisecond, "the slot frees once the stuck dial returns")
	kick()
	require.Eventually(t, func() bool { return dials.Load() == 2 }, 2*time.Second, 10*time.Millisecond)
}

func TestSelectForExcluding_NoFastFailoverToLastResortWhileRegularHealthy(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(s *MutableAutoSelect)
		wantErr bool
	}{
		{
			name: "failed member is still clean",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", 1)
			},
			wantErr: true,
		},
		{
			name: "failed member is soft-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", int(s.hist.softFailLimit))
			},
			wantErr: true,
		},
		{
			name: "failed member is hard-demoted",
			setup: func(s *MutableAutoSelect) {
				recordSuccess(s, "a", 100)
				addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, ubHard := range []bool{false, true} {
				s, _ := newLastResortMUR(t)
				recordSuccess(s, "ub", 4000)
				if ubHard {
					addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))
				}
				tt.setup(s)
				got, err := s.selectForExcluding("tcp", "a")
				if tt.wantErr {
					require.Errorf(t, err, "one failed dial on a healthy regular member must not route through the last resort (ub hard=%v)", ubHard)
					continue
				}
				require.NoError(t, err)
				assert.Equal(t, "ub", got.Tag())
			}
		})
	}
}

func TestSelectForExcluding_PrefersAnotherRegularMemberOverLastResort(t *testing.T) {
	s, obs := newTestMUR(t, "a", "b", "ub")
	obs["ub"].typeName = lConst.TypeUnbounded
	recordSuccess(s, "a", 100)
	recordSuccess(s, "b", 300)
	recordSuccess(s, "ub", 50)
	got, err := s.selectForExcluding("tcp", "a")
	require.NoError(t, err)
	assert.Equal(t, "b", got.Tag())
}

func TestRank_LastResortGetsNoSwitchPenaltyBoost(t *testing.T) {
	// The boost would double the hard limit for a member much faster than
	// its best alternative; a last resort must be held to the normal limit.
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "ub", 100)
	recordSuccess(s, "a", 990)
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	s.access.Lock()
	ranked := s.rankLocked(time.Now(), time.Time{})
	s.access.Unlock()
	for _, c := range ranked {
		if c.tag == "ub" {
			assert.Equal(t, demoteHard, c.demote)
		}
	}
}

func TestLastResortProbe_DiscardsOutcomeForReplacedURL(t *testing.T) {
	s, obs := newLastResortMUR(t)
	s.urlOverrides = map[string]string{"ub": "http://probe.test/old-token"}
	release := make(chan struct{})
	var dials atomic.Int32
	obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
		if dials.Add(1) == 1 {
			<-release
			return nil, errors.New("dial failed")
		}
		// The re-probe of the new URL stays pending, so any history entry
		// could only have come from the old probe.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), nil, true)
	s.access.Unlock()

	// A config update hands the member a new callback URL while the old
	// probe is still running.
	s.SetURLOverrides(map[string]string{"ub": "http://probe.test/new-token"})
	close(release)

	require.Eventually(t, func() bool { return dials.Load() == 2 }, 2*time.Second, 10*time.Millisecond,
		"the old probe has finished once the new URL's probe starts")
	s.access.Lock()
	_, ok := s.peekHistoryLocked("ub")
	s.access.Unlock()
	assert.False(t, ok, "an outcome for the replaced URL must not land in the new URL's history")
}

func TestRecordLastResortOutcome_DroppedAfterClose(t *testing.T) {
	s, _ := newLastResortMUR(t)
	s.cancel()
	s.recordLastResortOutcome("ub", s.probeURLForLocked("ub"), true, 100)
	s.access.Lock()
	_, ok := s.peekHistoryLocked("ub")
	s.access.Unlock()
	assert.False(t, ok, "an outcome landing after Close is not member evidence")
}

func TestRank_HardRegularOutranksHardLastResort(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 900)
	recordSuccess(s, "ub", 50)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))

	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag(), "when everything is hard-demoted, a regular member beats a faster last resort")
}

func TestSelectFor_LeavesHardStickyLastResortForHardRegular(t *testing.T) {
	s, _ := newLastResortMUR(t)
	recordSuccess(s, "a", 100)
	recordSuccess(s, "ub", 100)
	addUserFailureN(s, "a", int(s.hist.consecutiveFailLimit))
	got, err := s.selectFor("tcp")
	require.NoError(t, err)
	require.Equal(t, "ub", got.Tag())

	// The sticky last resort fails too; both are now hard, with equal delays
	// that the tolerance rule alone would keep sticky.
	addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))
	got, err = s.selectFor("tcp")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Tag())
}

// TestCheckOutbounds_KicksLastResortWhileAWaveIsRunning covers a config
// update landing while a probe wave holds probeMu: the external probe is
// dropped, but the last-resort probe that reports the new callback URL must
// still start.
func TestCheckOutbounds_KicksLastResortWhileAWaveIsRunning(t *testing.T) {
	s, obs := newLastResortMUR(t)
	s.defaultURL = "http://probe.test/"
	dialed := make(chan struct{}, 1)
	obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
		select {
		case dialed <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	s.runExternalProbe(nil)

	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "a busy probe wave held back the last-resort probe")
	}
}

// TestLastResortProbe_ReprobesWhenURLReplacedMidProbe covers a config update
// that replaces the callback URL while the old probe is still running: once
// the old probe finishes, the new URL is probed at once.
func TestLastResortProbe_ReprobesWhenURLReplacedMidProbe(t *testing.T) {
	s, obs := newLastResortMUR(t)
	s.urlOverrides = map[string]string{"ub": "http://probe.test/old-token"}
	release := make(chan struct{})
	var dials atomic.Int32
	obs["ub"].dial = func(ctx context.Context) (net.Conn, error) {
		if dials.Add(1) == 1 {
			<-release
			return nil, errors.New("old probe done")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), nil, true)
	s.access.Unlock()
	require.Eventually(t, func() bool { return dials.Load() == 1 }, 2*time.Second, 10*time.Millisecond)

	s.SetURLOverrides(map[string]string{"ub": "http://probe.test/new-token"})
	close(release)

	require.Eventually(t, func() bool { return dials.Load() == 2 }, 2*time.Second, 10*time.Millisecond,
		"the new callback URL must be probed as soon as the stale probe finishes")
}
