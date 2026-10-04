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
		delay, _, consec, _ := h.snapshot(time.Now(), s.hist.userFailureWindow)
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
		wantExhaustion bool
	}{
		{"latest last-resort probe succeeded", false, false},
		{"latest last-resort probe failed", true, true},
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
			recordSuccess(s, "ub", 4000)
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

// TestLastResortProbe_WatchdogFreesSlotWhenDialIgnoresDeadline covers an
// outbound whose dial ignores its context (broflake's SOCKS handshake reads
// without a deadline): the probe must still be recorded as failed and the
// in-flight slot freed so later probes run.
func TestLastResortProbe_WatchdogFreesSlotWhenDialIgnoresDeadline(t *testing.T) {
	orig := lastResortProbeBound
	lastResortProbeBound = func(protocolBehavior) time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { lastResortProbeBound = orig })

	s, obs := newLastResortMUR(t)
	s.defaultURL = "http://probe.test/"
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	var dials atomic.Int32
	obs["ub"].dial = func(context.Context) (net.Conn, error) {
		dials.Add(1)
		<-stuck
		return nil, errors.New("released")
	}

	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), nil, true)
	s.access.Unlock()

	require.Eventually(t, func() bool {
		s.access.Lock()
		defer s.access.Unlock()
		if s.lastResortInFlight["ub"] {
			return false
		}
		h, ok := s.peekHistoryLocked("ub")
		if !ok {
			return false
		}
		_, _, consec, _ := h.snapshot(time.Now(), s.hist.userFailureWindow)
		return consec == 1
	}, 2*time.Second, 10*time.Millisecond, "a dial that ignores its deadline must still end the probe as a failure")

	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), nil, true)
	s.access.Unlock()
	require.Eventually(t, func() bool { return dials.Load() == 2 }, 2*time.Second, 10*time.Millisecond,
		"the freed slot must let the next probe start")
}
