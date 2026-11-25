package group

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
)

func TestLocalHistory_RankDelayIsTheLowerMedian(t *testing.T) {
	now := time.Now()
	h := newLocalHistory()
	h.recordProbeSuccess(90, now)
	assert.EqualValues(t, 90, h.rankDelay, "a single sample is the delay")
	h.recordProbeSuccess(20, now)
	assert.EqualValues(t, 20, h.rankDelay, "of two samples, the slower is taken as noise")
	h.recordProbeSuccess(22, now)
	h.recordProbeSuccess(300, now)
	h.recordProbeSuccess(21, now)
	assert.EqualValues(t, 22, h.rankDelay, "outliers don't move the median")
	for range delaySamples {
		h.recordProbeSuccess(100, now)
	}
	assert.EqualValues(t, 100, h.rankDelay, "only the latest samples count")

	seeded := hydrateLocalHistory(&adapter.TagHistory{LastSuccessDelayMs: 40}, now, time.Minute)
	assert.EqualValues(t, 40, seeded.rankDelay, "a persisted delay seeds the samples")
}

func TestApplyStickiness_OneSlowProbeDoesNotSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a", "b")
		recordSuccess(s, "b", 100)
		h := s.historyForLocked("a")
		for range 3 {
			h.recordProbeSuccess(20, time.Now())
		}
		s.selections[streakTCP].tag.Store("a")
		h.recordProbeSuccess(300, time.Now())
		out, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", out.Tag(), "one slow probe of a member that is usually fast changes no exit")
	})
}

func TestDispatch_ForcedProbesTakeSlotsInRequestOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c", "d")
		s.cfg.probeConcurrency = 2
		for _, ob := range obs {
			ob.dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(time.Second), nil }
		}
		s.access.Lock()
		s.forceProbesLocked("d", "c", "b")
		var running []string
		for _, tag := range []string{"a", "b", "c", "d"} {
			if out, _ := s.members.Load(tag); s.inFlightProbes[out] != nil {
				running = append(running, tag)
			}
		}
		s.access.Unlock()
		assert.Equal(t, []string{"c", "d"}, running, "the first two requested run first, whatever the member order")
	})
}

// streakOnA gives a, the TCP selection, a failure streak at its limit and
// returns when it was reached. The probes it starts are left running.
func streakOnA(t *testing.T, s *MutableAutoSelect) time.Time {
	t.Helper()
	generation := s.networkGeneration.Load()
	s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "tcp", nil)
	time.Sleep(time.Second)
	since := time.Now()
	s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "tcp", nil)
	return since
}

func TestStreak_ReleaseWaitsForAHigherRankedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := streakOnA(t, s)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		s.access.Lock()
		require.True(t, s.passedSinceLocked("c", since), "c has passed")
		require.True(t, s.probePendingLocked("b"), "b's probe is still running")
		s.access.Unlock()
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag), "the release lands on the best member that passed")
	})
}

func TestStreak_ReleaseWaitIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		streakOnA(t, s)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		time.Sleep(replacementWaitFloor - 100*time.Millisecond)
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "b, slower than replacementWaitFloor, is not waited for")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestConfirmedFailure_ReplacementWaitsForAHigherRankedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		find := func() string {
			s.access.Lock()
			defer s.access.Unlock()
			return s.findProbedReplacementLocked(time.Now(), "tcp", "a", since)
		}
		assert.Empty(t, find(), "c's pass waits for b, ranked above it")
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", find(), "the replacement is the best member that passed")
	})
}

func TestHardDemotion_ReleaseWaitsForAHigherRankedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		for range s.hist.consecutiveFailLimit {
			seedProbe(s, "a", false, 0)
		}
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag), "the release lands on the best member that passed")
	})
}

func TestConfirmedFailure_WaitEndsWithoutAProbeCompleting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("dial timeout") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.requestFailureConfirmation(s.originAt("a", s.networkGeneration.Load()), "")
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		time.Sleep(replacementWaitFloor)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "the switch goes ahead once b's wait ends")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_WaitEndingReleasesAWaitingDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := streakOnA(t, s)
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Less(t, time.Since(since), streakProbeWait, "the dial goes ahead once b's wait ends, not at its own timeout")
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag))
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestAfterOutage_SwitchHeldForAHigherRankedProbeIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("dial timeout") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		seedProbe(s, "c", true, 30)
		s.access.Lock()
		s.forceProbesLocked("b")
		// c's pass ended an outage; a, the selection, is probed again.
		s.probeSelectionAfterOutageLocked("c", time.Now())
		s.access.Unlock()
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		time.Sleep(replacementWaitFloor)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a, failing its probe after the outage, still moves")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestAdvanceNetwork_FirstDelayReplacesTheOldNetworks(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	h := s.historyForLocked("a")
	for range 3 {
		h.recordProbeSuccess(20, time.Now())
	}
	s.access.Lock()
	s.advanceNetworkLocked()
	s.access.Unlock()
	assert.EqualValues(t, 20, h.rankDelay, "the old network's delay ranks a until it is probed")
	h.recordProbeSuccess(200, time.Now())
	assert.EqualValues(t, 200, h.rankDelay, "the first delay on the new network replaces the old network's")
}
