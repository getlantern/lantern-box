package group

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
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

	earlier := newLocalHistory()
	earlier.recordEarlierNetworkSuccess(10, now)
	assert.EqualValues(t, 10, earlier.rankDelay, "a delay from an earlier network ranks a member with none")
	earlier.recordProbeSuccess(200, now)
	assert.EqualValues(t, 200, earlier.rankDelay, "the first delay on this network replaces it")
	earlier.recordEarlierNetworkSuccess(10, now)
	assert.EqualValues(t, 200, earlier.rankDelay, "nor does it join this network's delays")

	seeded := hydrateLocalHistory(&adapter.TagHistory{LastSuccessDelayMs: 40}, now, time.Minute)
	assert.EqualValues(t, 40, seeded.rankDelay, "a persisted delay seeds the samples")
	seeded.recordProbeSuccess(200, now)
	assert.EqualValues(t, 200, seeded.rankDelay, "the first live delay replaces the persisted one")
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

// newSlotTestMUR returns a group of tags with slots probe slots, whose every
// probe takes a second.
func newSlotTestMUR(t *testing.T, slots int, tags ...string) *MutableAutoSelect {
	t.Helper()
	s, obs := newConfirmationTestMUR(t, tags...)
	s.cfg.probeConcurrency = slots
	for _, ob := range obs {
		ob.dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(time.Second), nil }
	}
	return s
}

// forceProbes requests each of batches in turn.
func forceProbes(s *MutableAutoSelect, batches ...[]string) {
	s.access.Lock()
	defer s.access.Unlock()
	for _, tags := range batches {
		s.forceProbesLocked(tags...)
	}
}

// probing returns which of tags have a probe in flight.
func probing(s *MutableAutoSelect, tags ...string) []string {
	s.access.Lock()
	defer s.access.Unlock()
	var running []string
	for _, tag := range tags {
		if out, _ := s.members.Load(tag); s.inFlightProbes[out] != nil {
			running = append(running, tag)
		}
	}
	return running
}

func TestDispatch_ForcedProbesTakeSlotsInRequestOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSlotTestMUR(t, 2, "a", "b", "c", "d")
		forceProbes(s, []string{"d", "c", "b"})
		assert.Equal(t, []string{"c", "d"}, probing(s, "a", "b", "c", "d"), "the first two requested run first, whatever the member order")
	})
}

func TestDispatch_ForcedProbesKeepTheirOrderPastOneQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSlotTestMUR(t, 1, "a", "b", "c", "d")
		forceProbes(s, []string{"a"}, []string{"c"}, []string{"d", "c"})
		time.Sleep(time.Second + 10*time.Millisecond)
		assert.Equal(t, []string{"d"}, probing(s, "c", "d"), "d, requested ahead of c, takes the next slot though c was queued first")
		time.Sleep(3 * time.Second)
		synctest.Wait()
	})
}

func TestDispatch_OverlappingForcedProbesKeepTheOrderTheyShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSlotTestMUR(t, 1, "a", "c", "b", "d")
		forceProbes(s, []string{"a"}, []string{"b", "c"}, []string{"c", "d"})
		var started []string
		for range 3 {
			time.Sleep(time.Second + 10*time.Millisecond)
			started = append(started, probing(s, "b", "c", "d")...)
		}
		assert.Equal(t, []string{"b", "c", "d"}, started, "both requests put b before c before d")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDispatch_ALaterRequestReordersQueuedProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSlotTestMUR(t, 1, "a", "b", "c")
		forceProbes(s, []string{"a"}, []string{"c", "b"}, []string{"b", "c"})
		time.Sleep(time.Second + 10*time.Millisecond)
		assert.Equal(t, []string{"b"}, probing(s, "b", "c"), "the latest request puts b ahead of c")
		time.Sleep(3 * time.Second)
		synctest.Wait()
	})
}

func TestScheduler_EarlierNetworksDelayLeavesTheResetPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		obs["a"].dial = denyDial
		s.access.Lock()
		recordSuccess(s, "ub", 20)
		s.access.Unlock()
		release := make(chan struct{})
		var dials atomic.Int32
		obs["ub"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
				return slowProbeResponderConn(30 * time.Millisecond), nil
			}
			return slowProbeResponderConn(200 * time.Millisecond), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		s.InterfaceUpdated(context.Background())
		synctest.Wait()
		close(release)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		s.access.Lock()
		s.forceProbesLocked("ub")
		s.access.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load(), "ub was probed again on the new network")
		s.access.Lock()
		defer s.access.Unlock()
		h, ok := s.peekHistoryLocked("ub")
		require.True(t, ok)
		assert.EqualValues(t, 200, h.rankDelayMs(), "the first delay measured on the new network replaces the old ones")
	})
}

// dialAsync dials TCP through s in the background and reports the dial's
// error.
func dialAsync(ctx context.Context, s *MutableAutoSelect) <-chan error {
	dialed := make(chan error, 1)
	go func() {
		conn, err := s.DialContext(ctx, "tcp", metadata.Socksaddr{})
		if err == nil {
			conn.Close()
		}
		dialed <- err
	}()
	return dialed
}

// replacementOffA returns the member findProbedReplacementLocked moves the
// TCP selection, a, to for failures since since, or "" while it has none or
// waits.
func replacementOffA(s *MutableAutoSelect, since time.Time) string {
	s.access.Lock()
	defer s.access.Unlock()
	tag, _ := s.findProbedReplacementLocked(time.Now(), "tcp", "a", since)
	return tag
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
		assert.Empty(t, replacementOffA(s, since), "c's pass waits for b, ranked above it")
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since), "the replacement is the best member that passed")
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

func TestHardDemotion_WaitCountsFromTheRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(2900 * time.Millisecond)
			return nil, errors.New("unreachable")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		for range s.hist.consecutiveFailLimit - 1 {
			seedProbe(s, "a", false, 0)
		}
		seedProbe(s, "b", true, 20)
		time.Sleep(time.Second)
		// c passes while a's last probe, which hard-demotes it, runs on.
		s.access.Lock()
		s.forceProbesLocked("a", "c")
		s.access.Unlock()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass from before the release waits for b's probe")
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag), "the release lands on the best member that passed")
	})
}

func TestHardDemotion_TCPAndUDPWaitSeparately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(2500 * time.Millisecond)
			return slowProbeResponderConn(10 * time.Millisecond), nil
		}
		s.access.Lock()
		s.forceProbesLocked("b")
		s.access.Unlock()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.selections[streakUDP].tag.Store("a")
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
		time.Sleep(1100 * time.Millisecond)
		_, _, err = s.selectOutbound("udp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "UDP's release waits from its own start, not TCP's")
		time.Sleep(3 * time.Second)
		synctest.Wait()
	})
}

// probeThenMoveB probes b and c, then changes b's probe URL, so the pass of
// b's old URL is obsolete while its new probe runs. b stays ranked above c.
func probeThenMoveB(t *testing.T, s *MutableAutoSelect, obs map[string]*mockOutbound) {
	t.Helper()
	obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(50 * time.Millisecond), nil }
	obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
	s.access.Lock()
	s.forceProbesLocked("b", "c")
	s.access.Unlock()
	time.Sleep(110 * time.Millisecond)
	synctest.Wait()
	obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(25 * time.Millisecond), nil }
	s.SetURLOverrides(map[string]string{"b": "http://probe.test/moved"})
}

func TestConfirmedFailure_ReplacementWaitsForAProbeOfANewURL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		since := time.Now()
		probeThenMoveB(t, s, obs)
		assert.Empty(t, replacementOffA(s, since), "b's pass of its old URL is no outcome; c's pass waits for its new probe")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since))
	})
}

func TestHardDemotion_ReleaseWaitsForAProbeOfANewURL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
		for range s.hist.consecutiveFailLimit {
			seedProbe(s, "a", false, 0)
		}
		probeThenMoveB(t, s, obs)
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "b's pass of its old URL is no outcome; c's pass waits for its new probe")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag))
	})
}

func TestConfirmedFailure_ProbeOfAnOldURLDoesNotEndTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		var moved atomic.Bool
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			if moved.Load() {
				return slowProbeResponderConn(20 * time.Millisecond), nil
			}
			return slowProbeResponderConn(time.Second), nil
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(50 * time.Millisecond)
		moved.Store(true)
		s.SetURLOverrides(map[string]string{"b": "http://probe.test/moved"})
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "c's pass waits for b's probe of its new URL, queued behind the old one")
		time.Sleep(time.Second - 300*time.Millisecond + 60*time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since), "the replacement is the best member that passed")
	})
}

func TestConfirmedFailure_WaitCountsFromAnEarlierProbesStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.access.Lock()
		s.forceProbesLocked("b")
		s.access.Unlock()
		time.Sleep(50 * time.Millisecond)
		// b's probe, under way since before, decides the replacement too.
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("c")
		s.access.Unlock()
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "c's pass waits for b, ranked above it")
		time.Sleep(replacementWaitFloor - 150*time.Millisecond)
		assert.Equal(t, "c", replacementOffA(s, since), "b is waited for from its probe's start, not from each retry")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestConfirmedFailure_WaitCoversAProbesHandshake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(500 * time.Millisecond)
			return slowProbeResponderConn(10 * time.Millisecond), nil
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.access.Lock()
		s.forceProbesLocked("b")
		s.access.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "c's pass waits for b, whose dial takes most of its probe")
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since), "the replacement is the best member that passed")
	})
}

func TestConfirmedFailure_WaitOutlastsASlowProbesStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1700 * time.Millisecond)
			return slowProbeResponderConn(10 * time.Millisecond), nil
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1500 * time.Millisecond)
			return slowProbeResponderConn(100 * time.Millisecond), nil
		}
		s.access.Lock()
		s.forceProbesLocked("b")
		s.access.Unlock()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(1650 * time.Millisecond)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "c's pass at 1.6s waits for b, whose probes take 1.7s")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since), "the replacement is the best member that passed")
	})
}

func TestConfirmedFailure_WaitIsBoundedFromTheFirstPassForTheRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1700 * time.Millisecond)
			return slowProbeResponderConn(10 * time.Millisecond), nil
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1500 * time.Millisecond)
			return slowProbeResponderConn(100 * time.Millisecond), nil
		}
		// c, the fallback, was passing before the release began.
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		time.Sleep(1650 * time.Millisecond)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "c's first pass for the release, at 1.6s, waits for b")
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", replacementOffA(s, since), "the replacement is the best member that passed")
	})
}

func TestSameProbeEndpoint(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{"http://probe.test/cb?token=1", "http://probe.test/cb?token=2", true},
		{"http://probe.test/cb", "https://probe.test/cb", false},
		{"http://probe.test/cb", "http://probe.test:8080/cb", false},
		{"http://probe.test/cb", "http://other.test/cb", false},
		{"http://probe.test/a/b", "http://probe.test/a%2Fb", false},
		{"http://probe.test/cb", "http://user:pass@probe.test/cb", false},
	} {
		assert.Equal(t, tt.same, sameProbeEndpoint(tt.a, tt.b), "%s vs %s", tt.a, tt.b)
	}
}

// rankDelayAfterOverride returns b's rank delay once it passes a 100ms probe
// of override, from a seeded 20ms.
func rankDelayAfterOverride(t *testing.T, override string) uint32 {
	t.Helper()
	var delay uint32
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.SetURLOverrides(map[string]string{"b": override})
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		s.access.Lock()
		defer s.access.Unlock()
		h, ok := s.peekHistoryLocked("b")
		require.True(t, ok)
		delay = h.rankDelayMs()
	})
	return delay
}

func TestSetURLOverrides_NewEndpointsDelaysReplaceTheOldOnes(t *testing.T) {
	assert.EqualValues(t, 100, rankDelayAfterOverride(t, "http://moved.test/"), "b is ranked by its delay to the new server")
	assert.EqualValues(t, 100, rankDelayAfterOverride(t, "http://probe.test/moved"), "b is ranked by its delay to the new path")
}

func TestSetURLOverrides_NewTokenKeepsTheDelays(t *testing.T) {
	assert.EqualValues(t, 20, rankDelayAfterOverride(t, "http://probe.test/?token=2"), "one sample doesn't outweigh b's earlier ones")
}

func TestConfirmedFailure_WaitIsBoundedFromTheFirstPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2900 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		// b's probe now can't decide the release; its next waits behind it.
		s.SetURLOverrides(map[string]string{"b": "http://probe.test/?token=2"})
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, replacementOffA(s, since), "c's pass waits for b, ranked above it")
		time.Sleep(490 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, replacementOffA(s, since), "c's pass waits for b, ranked above it")
		s.access.Lock()
		s.forceProbesLocked("c")
		s.access.Unlock()
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "c", replacementOffA(s, since), "c passing again doesn't extend the wait past a second from its first pass")
		time.Sleep(6 * time.Second)
		synctest.Wait()
	})
}

func TestConfirmedFailure_WaitEndsWithoutAProbeCompleting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		confirmedSwitchHeldOffA(t, s, obs)
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
		assert.Less(t, time.Since(since), replacementProbeWait, "the dial goes ahead once b's wait ends, not at its own timeout")
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag))
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_HardDemotionWaitHoldsADial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		var dialedA atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			dialedA.Add(1)
			return nil, errors.New("unreachable")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		for range s.hist.consecutiveFailLimit {
			seedProbe(s, "a", false, 0)
		}
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Zero(t, dialedA.Load(), "the dial waits for the release instead of going through a")
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag), "the release lands on the best member that passed")
	})
}

func TestDialContext_ConfirmedSwitchWaitHoldsADial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		var held atomic.Bool
		var dialedA atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if held.Load() {
				dialedA.Add(1)
			}
			return nil, errors.New("dial timeout")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(200 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.requestFailureConfirmation(s.originAt("a", s.networkGeneration.Load()), "")
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		held.Store(true)
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Zero(t, dialedA.Load(), "the dial waits for the switch instead of going through a")
		assert.Equal(t, "b", loadString(&s.selections[streakTCP].tag), "the switch lands on the best member that passed")
	})
}

func TestDialContext_DueConfirmedSwitchIsAppliedFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		var held atomic.Bool
		var dialedA atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if held.Load() {
				dialedA.Add(1)
			}
			return nil, errors.New("dial timeout")
		}
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.requestFailureConfirmation(s.originAt("a", s.networkGeneration.Load()), "")
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
		// The dial comes in after b's wait ends, before replacementWake runs.
		s.access.Lock()
		s.replacementWake.Stop()
		s.replacementWake = nil
		s.access.Unlock()
		time.Sleep(replacementWaitFloor)
		held.Store(true)
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Zero(t, dialedA.Load(), "the due switch is applied before the dial")
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag))
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_HeldAfterOutageSwitchStopsHoldingOnceTheSelectionAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		afterOutageHeldOnA(t, s, obs)
		time.Sleep(10 * time.Millisecond)
		s.access.Lock()
		s.historyForLocked("a").noteUserResponse(streakTCP, "", time.Now(), time.Now())
		s.access.Unlock()
		obs["a"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(0), nil }
		start := time.Now()
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Zero(t, time.Since(start), "a, answering again, takes the dial at once")
		assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag))
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_WaitingDialGoesAheadOnceTheSelectionAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		afterOutageHeldOnA(t, s, obs)
		obs["a"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(0), nil }
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		dialed := dialAsync(ctx, s)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		s.noteAnswer(s.originAt("a", s.networkGeneration.Load()), "tcp", "", 0, time.Now(), time.Now())
		assert.NoError(t, <-dialed, "a, answering again, takes the waiting dial")
		assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag))
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_ConfirmedSwitchInCooldownHoldsNoDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("dial timeout") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(500 * time.Millisecond), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.access.Lock()
		s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = time.Now(), "c"
		s.access.Unlock()
		s.requestFailureConfirmation(s.originAt("a", s.networkGeneration.Load()), "")
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		s.access.Lock()
		require.Equal(t, "a", s.pendingConfirmedFailure.tag, "the switch off a is pending")
		s.access.Unlock()
		start := time.Now()
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		if err == nil {
			conn.Close()
		}
		assert.Zero(t, time.Since(start), "no probe can let the switch through during the cooldown")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_UDPAnswerLeavesTCPDialsWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		afterOutageHeldOnA(t, s, obs)
		s.access.Lock()
		require.Equal(t, "a", s.pendingConfirmedFailure.tag, "c's pass waits for b, ranked above it")
		changed := s.probeChanged
		s.access.Unlock()
		s.noteAnswer(s.originAt("a", s.networkGeneration.Load()), "udp", "", 0, time.Now(), time.Now())
		select {
		case <-changed:
			t.Error("a UDP answer, which leaves the TCP switch held, woke the dials waiting on it")
		default:
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_ClosedGroupSettlesNoPendingSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		confirmedSwitchHeldOffA(t, s, obs)
		require.NoError(t, s.Close())
		time.Sleep(replacementWaitMax)
		_, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		assert.ErrorIs(t, err, net.ErrClosed)
		assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "a closed group switches nothing")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_UnsupportedNetworkSettlesNoPendingSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		confirmedSwitchHeldOffA(t, s, obs)
		start := time.Now()
		_, err := s.DialContext(context.Background(), "tcp4", metadata.Socksaddr{})
		assert.Error(t, err)
		assert.Zero(t, time.Since(start), "a network the group rejects waits for nothing")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_DialOutwaitingItsBudgetTakesThePassedMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(2500 * time.Millisecond)
			return slowProbeResponderConn(10 * time.Millisecond), nil
		}
		s.access.Lock()
		s.forceProbesLocked("b")
		s.access.Unlock()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		var held atomic.Bool
		var dialedA atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if held.Load() {
				dialedA.Add(1)
			}
			return nil, errors.New("blocked")
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1800 * time.Millisecond)
			return slowProbeResponderConn(100 * time.Millisecond), nil
		}
		streakOnA(t, s)
		held.Store(true)
		conn, err := s.DialContext(context.Background(), "tcp", metadata.Socksaddr{})
		require.NoError(t, err)
		conn.Close()
		assert.Zero(t, dialedA.Load(), "the dial, its wait spent, goes to c, which passed, not to a")
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag))
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_WaitingDialGoesAheadOnceAnAnswerEndsTheStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		streakOnA(t, s)
		obs["a"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(0), nil }
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		dialed := dialAsync(ctx, s)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		s.noteAnswer(s.originAt("a", s.networkGeneration.Load()), "tcp", "", 0, time.Now(), time.Now())
		assert.NoError(t, <-dialed, "a, answering again, takes the waiting dial")
		time.Sleep(3 * time.Second)
		synctest.Wait()
	})
}

// afterOutageHeldOnA leaves the switch off the TCP selection, a, waiting
// for b's 2s probe: c's pass ended an outage, and a failed its probe after.
func afterOutageHeldOnA(t *testing.T, s *MutableAutoSelect, obs map[string]*mockOutbound) {
	t.Helper()
	obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("dial timeout") }
	obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
	seedProbe(s, "c", true, 30)
	s.access.Lock()
	s.forceProbesLocked("b")
	s.probeSelectionAfterOutageLocked("c", time.Now())
	s.access.Unlock()
	synctest.Wait()
	require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
}

// confirmedSwitchHeldOffA leaves a's confirmed-failure switch to c waiting
// for b's 2s probe.
func confirmedSwitchHeldOffA(t *testing.T, s *MutableAutoSelect, obs map[string]*mockOutbound) {
	t.Helper()
	obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("dial timeout") }
	obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
	obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
	s.requestFailureConfirmation(s.originAt("a", s.networkGeneration.Load()), "")
	time.Sleep(110 * time.Millisecond)
	synctest.Wait()
	require.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "c's pass waits for b, ranked above it")
}

func TestEndReleaseWaits_MakesThePendingConfirmedSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		confirmedSwitchHeldOffA(t, s, obs)
		s.access.Lock()
		s.endReleaseWaitsLocked("tcp")
		s.access.Unlock()
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a dial out of time takes c, which passed")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestEndReleaseWaits_EndsAWaitNotYetStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		streakOnA(t, s)
		// c passes, and nothing has looked at a's release since.
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		s.access.Lock()
		s.endReleaseWaitsLocked("tcp")
		s.access.Unlock()
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a dial out of time takes c, which passed")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestEndReleaseWaits_EndsEveryReleasePathsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		streakOnA(t, s)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		// a, at its streak's limit, is hard-demoted too.
		for range s.hist.consecutiveFailLimit {
			seedProbe(s, "a", false, 0)
		}
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		// c passes again, for the hard demotion, and nothing has looked since.
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakTCP].tag))
		s.access.Lock()
		s.endReleaseWaitsLocked("tcp")
		s.access.Unlock()
		_, _, err = s.selectOutbound("tcp", "")
		require.NoError(t, err)
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a dial out of time takes c, whichever path releases a")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestEndReleaseWaits_WakeUnderWayKeepsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		s.access.Lock()
		s.endReleaseWaitsLocked("udp")
		// A pause began and is being left: its flag is cleared, the wake's
		// callback hasn't run.
		s.pausedSince = time.Now()
		s.access.Unlock()
		_, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "evidence from before the pause releases nothing")
		s.access.Lock()
		s.pausedSince = time.Time{}
		s.access.Unlock()
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestDialContext_WaitingDialGoesAheadOnceTheSelectionMoves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		confirmedSwitchHeldOffA(t, s, obs)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		dialed := dialAsync(ctx, s)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		// Something else, such as a trial answering, moves the selection.
		s.access.Lock()
		s.switchSelectionLocked("tcp", "a", "c", switchReason{text: "test"})
		s.access.Unlock()
		assert.NoError(t, <-dialed, "the waiting dial goes to c at once")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_WaitEndingDuringAPauseKeepsTheUDPSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		pm.NetworkPause()
		time.Sleep(replacementWaitMax)
		synctest.Wait()
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "a wait ending while paused releases nothing")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_DialDuringAPauseKeepsTheUDPSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		pm.NetworkPause()
		time.Sleep(2 * replacementWaitMax)
		_, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "a dial while paused releases nothing from before the pause")
		pm.NetworkWake()
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

// udpStreakHeldOnA gives a, the UDP selection, a failure streak whose
// release to c waits for b's 2s probe.
func udpStreakHeldOnA(t *testing.T, s *MutableAutoSelect, obs map[string]*mockOutbound) {
	t.Helper()
	obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
	obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(2 * time.Second), nil }
	obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
	s.selections[streakUDP].tag.Store("a")
	generation := s.networkGeneration.Load()
	s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
	time.Sleep(time.Second)
	s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
	time.Sleep(110 * time.Millisecond)
	synctest.Wait()
	require.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "c's pass waits for b, ranked above it")
}

func TestStreak_PauseHoldIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		pm.NetworkPause()
		time.Sleep(pausedReleaseHold + 100*time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakUDP].tag), "a pause latched past pausedReleaseHold keeps a no longer")
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_WakeUnderWayKeepsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		// The pause manager has cleared its flag; the wake's callback hasn't
		// run yet.
		s.access.Lock()
		s.pausedSince = time.Now()
		s.access.Unlock()
		time.Sleep(replacementWaitMax)
		_, _, err := s.selectOutbound("udp", "")
		require.NoError(t, err)
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "evidence from before the pause releases nothing")
		s.access.Lock()
		s.pausedSince = time.Time{}
		s.access.Unlock()
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestSwitch_EndsTheNetworksReleaseWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(3 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		since := time.Now()
		s.access.Lock()
		s.forceProbesLocked("b", "c")
		s.access.Unlock()
		// b's probe now can't decide the release; its next waits behind it.
		s.SetURLOverrides(map[string]string{"b": "http://probe.test/?token=2"})
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Empty(t, replacementOffA(s, since), "c's pass waits for b, ranked above it")
		// The selection moves on and back before the wait would have ended.
		s.access.Lock()
		s.switchSelectionLocked("tcp", "a", "c", switchReason{text: "test"})
		s.switchSelectionLocked("tcp", "c", "a", switchReason{text: "test"})
		s.access.Unlock()
		time.Sleep(replacementWaitMax)
		synctest.Wait()
		assert.Empty(t, replacementOffA(s, since), "a release after the switch waits anew")
		time.Sleep(7 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_WaitEndedDuringAPauseRestartsAfterIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		udpStreakHeldOnA(t, s, obs)
		pm.NetworkPause()
		// A dial's budget ran out as the pause began.
		s.access.Lock()
		s.endReleaseWaitsLocked("udp")
		s.access.Unlock()
		time.Sleep(time.Second)
		pm.NetworkWake()
		synctest.Wait()
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "after the wake, c's pass from before waits for b again")
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_WaitRestartsAfterAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		var woke atomic.Bool
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			if woke.Load() {
				return slowProbeResponderConn(100 * time.Millisecond), nil
			}
			return slowProbeResponderConn(2 * time.Second), nil
		}
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.selections[streakUDP].tag.Store("a")
		generation := s.networkGeneration.Load()
		s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
		time.Sleep(time.Second)
		s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "c's pass waits for b, ranked above it")
		pm.NetworkPause()
		time.Sleep(2 * replacementWaitMax)
		woke.Store(true)
		pm.NetworkWake()
		synctest.Wait()
		s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
		assert.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "c's pass from before the pause waits for b's probe after it")
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", loadString(&s.selections[streakUDP].tag), "the release lands on b once it passes")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestStreak_WaitAfterAPauseIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("blocked") }
		obs["b"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(4 * time.Second), nil }
		obs["c"].dial = func(context.Context) (net.Conn, error) { return slowProbeResponderConn(100 * time.Millisecond), nil }
		s.selections[streakUDP].tag.Store("a")
		generation := s.networkGeneration.Load()
		s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
		time.Sleep(time.Second)
		s.recordUserFailure(s.originAt("a", generation), adapter.UserFailureReset, time.Now(), "udp", nil)
		time.Sleep(110 * time.Millisecond)
		synctest.Wait()
		require.Equal(t, "a", loadString(&s.selections[streakUDP].tag), "c's pass waits for b, ranked above it")
		pm.NetworkPause()
		time.Sleep(2 * replacementWaitMax)
		pm.NetworkWake()
		time.Sleep(replacementWaitMax)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakUDP].tag), "with nothing else retrying it, the release waits no longer than replacementWaitMax from the wake")
		time.Sleep(4 * time.Second)
		synctest.Wait()
	})
}

func TestAfterOutage_SwitchHeldForAHigherRankedProbeIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		afterOutageHeldOnA(t, s, obs)
		time.Sleep(replacementWaitFloor)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a, failing its probe after the outage, still moves")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestAfterOutage_SwitchHeldThroughAPauseIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		pm := withPauseManager(s)
		afterOutageHeldOnA(t, s, obs)
		pm.NetworkPause()
		time.Sleep(time.Second)
		pm.NetworkWake()
		time.Sleep(replacementWaitMax + 100*time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "c", loadString(&s.selections[streakTCP].tag), "a, failing its probe again after the wake, still moves")
		time.Sleep(2 * time.Second)
		synctest.Wait()
	})
}

func TestAfterOutage_HeldSwitchDropsOnceTheSelectionAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newConfirmationTestMUR(t, "a", "b", "c")
		afterOutageHeldOnA(t, s, obs)
		time.Sleep(10 * time.Millisecond)
		s.access.Lock()
		s.historyForLocked("a").noteUserResponse(streakTCP, "", time.Now(), time.Now())
		s.access.Unlock()
		time.Sleep(replacementWaitFloor)
		synctest.Wait()
		assert.Equal(t, "a", loadString(&s.selections[streakTCP].tag), "a, answering traffic since c's pass, stays")
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
