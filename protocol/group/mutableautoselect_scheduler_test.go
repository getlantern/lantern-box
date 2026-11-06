package group

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/getlantern/lantern-box/adapter"
	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
	"github.com/stretchr/testify/require"
)

func requestTestProbes(s *MutableAutoSelect, tags []string, need probeNeed) {
	s.access.Lock()
	defer s.access.Unlock()
	s.requestProbesLocked(tags, need, false)
	s.dispatchProbesLocked()
}

func slotsUsed(s *MutableAutoSelect) int {
	s.access.Lock()
	defer s.access.Unlock()
	return s.probeSlotsUsed
}

func newSchedulerTest(t *testing.T, tags ...string) (*MutableAutoSelect, map[string]*mockOutbound) {
	s, obs := newTestMUR(t, tags...)
	s.defaultURL = "http://probe.test/"
	for _, out := range obs {
		out.dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
	}
	return s, obs
}

func TestSchedulerSharedOwnershipAndConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		first, second := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-first
				return probeResponderConn(), nil
			}
			<-second
			return nil, errors.New("failed confirmation")
		}
		recordSuccess(s, "b", 100)
		s.stickyTag.tcp.Store("a")
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		s.requestFailureConfirmation("a", "b", s.networkGeneration.Load())
		s.CheckOutbounds()
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		close(first)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.Equal(t, "a", s.Now(), "attempt preceding the confirmation cannot switch selection")
		close(second)
		synctest.Wait()
		require.Equal(t, "b", s.Now())
	})
}

func TestSchedulerConfirmationBypassesConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b", "c")
		s.cfg.probeConcurrency = 1
		release := map[string]chan struct{}{}
		started := make(chan string, 3)
		for tag, out := range obs {
			release[tag] = make(chan struct{})
			out.dial = func(context.Context) (net.Conn, error) {
				started <- tag
				<-release[tag]
				return probeResponderConn(), nil
			}
		}
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		require.Equal(t, "a", <-started)
		requestTestProbes(s, []string{"b"}, probeIfStale)
		s.stickyTag.tcp.Store("c")
		s.requestFailureConfirmation("c", "", s.networkGeneration.Load())
		synctest.Wait()
		require.Equal(t, "c", <-started)
		require.Empty(t, started)
		require.Equal(t, 1, s.probeSlotsUsed)
		close(release["c"])
		synctest.Wait()
		require.Empty(t, started)
		require.Equal(t, 1, s.probeSlotsUsed)
		close(release["a"])
		synctest.Wait()
		require.Equal(t, "b", <-started)
		close(release["b"])
		synctest.Wait()
		require.Zero(t, s.probeSlotsUsed)
	})
}

func TestSchedulerCanceledObjectRemainsBusyAcrossFlaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		s.probing = true
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) { dials.Add(1); <-release; return probeResponderConn(), nil }
		s.CheckOutbounds()
		synctest.Wait()
		for range 4 {
			s.InterfaceUpdated(context.Background())
			s.stickyTag.tcp.Store("a")
			s.requestFailureConfirmation("a", "", s.networkGeneration.Load())
			s.CheckOutbounds()
			synctest.Wait()
			require.EqualValues(t, 1, dials.Load())
		}
		require.Equal(t, 1, s.probeSlotsUsed, "a canceled dial keeps its slot until it returns")
		require.Len(t, s.inFlightProbes, 1)
		close(release)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.Empty(t, s.inFlightProbes)
		require.Zero(t, s.probeSlotsUsed)
		require.Zero(t, s.history.Load("a").ConsecutiveFailures)
	})
}

func TestSchedulerRemoveReaddSameObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		s.outboundMgr = &mockOutboundManager{outbounds: map[string]A.Outbound{"a": obs["a"]}}
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
				return nil, errors.New("old membership")
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		old := s.probeMembers["a"].membershipID
		_, err := s.Remove("a")
		require.NoError(t, err)
		_, err = s.Add("a")
		require.NoError(t, err)
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		require.NotEqual(t, old, s.probeMembers["a"].membershipID)
		close(release)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.Zero(t, s.history.Load("a").ConsecutiveFailures)
	})
}

func TestSchedulerURLChangeRejectsOldOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		first, second := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-first
			} else {
				<-second
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		s.SetURLOverrides(map[string]string{"a": "http://new.test/"})
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		close(first)
		synctest.Wait()
		require.Nil(t, s.history.Load("a"))
		require.EqualValues(t, 2, dials.Load())
		close(second)
		synctest.Wait()
		require.Equal(t, "http://new.test/", s.probeMembers["a"].outcome.url)
	})
}

func TestSchedulerPauseCancelsAndWakeDispatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		pm := withPauseManager(s)
		var dials atomic.Int32
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		pm.DevicePause()
		pm.NetworkPause()
		synctest.Wait()
		require.Nil(t, s.history.Load("a"))
		pm.DeviceWake()
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		pm.NetworkWake()
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.NotNil(t, s.probeMembers["a"].outcome)
		require.NoError(t, s.Close())
		pm.NetworkPause()
		pm.NetworkWake()
	})
}

func TestSchedulerWatchdogReleasesSlotButKeepsObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		s.cfg.probeConcurrency = 1
		recordSuccess(s, "a", 1)
		recordSuccess(s, "b", 200)
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		time.Sleep(probeBound(behaviorFor(obs["a"].Type())))
		synctest.Wait()
		require.True(t, s.stuckProbeLocked(obs["a"]))
		require.EqualValues(t, 1, s.history.Load("a").ConsecutiveFailures)
		selected, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		require.Equal(t, "b", selected.Tag())
		require.False(t, s.eligibleReplacementLocked(time.Now(), "a"))
		require.NotNil(t, s.probeMembers["b"].outcome)
		s.CheckOutbounds()
		synctest.Wait()
		require.Equal(t, probeNotNeeded, s.probeMembers["a"].need, "a fresh failure satisfies a stale-only request")
		time.Sleep(probeFreshnessWindow)
		s.CheckOutbounds()
		synctest.Wait()
		require.Equal(t, probeIfStale, s.probeMembers["a"].need)
		require.EqualValues(t, 1, dials.Load())
		close(release)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.Zero(t, s.history.Load("a").ConsecutiveFailures)
	})
}

func TestSchedulerCanceledOperationBecomesStuckWithoutFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) { <-release; return nil, errors.New("released") }
		s.CheckOutbounds()
		synctest.Wait()
		pm := withPauseManager(s)
		pm.NetworkPause()
		synctest.Wait()
		require.Equal(t, 1, slotsUsed(s), "a canceled dial keeps its slot")
		time.Sleep(probeBound(behaviorFor(obs["a"].Type())))
		synctest.Wait()
		require.True(t, s.stuckProbeLocked(obs["a"]))
		require.Zero(t, slotsUsed(s), "the watchdog frees a canceled attempt's slot")
		require.Nil(t, s.history.Load("a"))
		close(release)
		synctest.Wait()
	})
}

func TestSchedulerURLTestPartialResultsDoNotCancelWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		release := make(chan struct{})
		var canceled atomic.Bool
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			select {
			case <-release:
				return probeResponderConn(), nil
			case <-ctx.Done():
				canceled.Store(true)
				return nil, ctx.Err()
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan map[string]uint16, 1)
		go func() { results, _ := s.URLTest(ctx); done <- results }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		results := <-done
		require.Contains(t, results, "a")
		require.NotContains(t, results, "b")
		require.False(t, canceled.Load())
		close(release)
		synctest.Wait()
		require.NotNil(t, s.probeMembers["b"].outcome)
	})
}

func TestSchedulerCloseUnblocksWaiterAndDropsLateReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) { <-release; return probeResponderConn(), nil }
		done := make(chan struct{})
		go func() { s.URLTest(t.Context()); close(done) }()
		synctest.Wait()
		s.Close()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("close did not end waiter")
		}
		close(release)
		synctest.Wait()
		require.Nil(t, s.history.Load("a"))
	})
}

func TestSchedulerFilteredFreshnessIsRuntimeOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		s.history.Store("a", &adapter.TagHistory{LastSuccessDelayMs: 1, LastOutcomeAt: time.Now(), UpdatedAt: time.Now()})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) { dials.Add(1); return probeResponderConn(), nil }
		s.CheckOutbounds()
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		s.CheckOutbounds()
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		requestTestProbes(s, nil, probeForced)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		time.Sleep(probeFreshnessWindow)
		s.CheckOutbounds()
		synctest.Wait()
		require.EqualValues(t, 3, dials.Load())
	})
}

func TestSchedulerLadderBudgetStartsWhenSlotAvailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		s.cfg.probeConcurrency = 1
		s.cfg.ladderTotalBudget = time.Second
		first, second := make(chan struct{}), make(chan struct{})
		var canceled atomic.Bool
		obs["a"].dial = func(context.Context) (net.Conn, error) { <-first; return nil, errors.New("failed") }
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			select {
			case <-second:
				return probeResponderConn(), nil
			case <-ctx.Done():
				canceled.Store(true)
				return nil, ctx.Err()
			}
		}
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		done := make(chan struct{})
		go func() { s.runLadder("a", s.networkGeneration.Load()); close(done) }()
		synctest.Wait()
		s.access.Lock()
		budgetStarted := !s.ladderWaiter.deadline.IsZero()
		s.access.Unlock()
		require.False(t, budgetStarted)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("budget started before a slot was available")
		default:
		}
		close(first)
		synctest.Wait()
		s.access.Lock()
		budgetStarted = !s.ladderWaiter.deadline.IsZero()
		s.access.Unlock()
		require.True(t, budgetStarted)
		time.Sleep(time.Second)
		synctest.Wait()
		<-done
		require.True(t, exhausted(s))
		require.False(t, canceled.Load())
		close(second)
		synctest.Wait()
	})
}

func TestSchedulerLastResortSurvivesNetworkChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		obs["a"].dial = denyDial
		release := make(chan struct{})
		var dials atomic.Int32
		obs["ub"].dial = func(context.Context) (net.Conn, error) { dials.Add(1); <-release; return probeResponderConn(), nil }
		s.CheckOutbounds()
		synctest.Wait()
		s.InterfaceUpdated(context.Background())
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		close(release)
		synctest.Wait()
		require.True(t, s.probeMembers["ub"].outcome.success)
		require.Positive(t, s.history.Load("ub").LastSuccessDelayMs)
		s.access.Lock()
		eligible := s.eligibleReplacementLocked(time.Now(), "ub")
		var delayMs uint32
		for _, candidate := range s.rankLocked(time.Now()) {
			if candidate.tag == "ub" {
				delayMs = candidate.delayMs
			}
		}
		s.access.Unlock()
		require.True(t, eligible)
		require.Positive(t, delayMs)
		s.runProbeCycle()
		require.False(t, exhausted(s))
	})
}

func TestSchedulerWaiterDoesNotUseEarlierCompletionAtSameClockInstant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newSchedulerTest(t, "a")
		s.CheckOutbounds()
		synctest.Wait()
		s.access.Lock()
		forcedFinished := s.waiterFinishedLocked(s.newProbeWaiterLocked(false))
		filteredFinished := s.waiterFinishedLocked(s.newProbeWaiterLocked(true))
		s.access.Unlock()
		require.False(t, forcedFinished)
		require.True(t, filteredFinished)
	})
}

func TestSchedulerURLTestShouldWaitAcrossPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		pm := withPauseManager(s)
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
		done := make(chan struct{})
		go func() { s.URLTest(t.Context()); close(done) }()
		synctest.Wait()
		pm.NetworkPause()
		synctest.Wait()
		select {
		case <-done:
			t.Error("URLTest returned on pause before context end or Close")
		default:
		}
		s.Close()
		synctest.Wait()
	})
}
func TestSchedulerURLTestRetainsGatheredSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan map[string]uint16, 1)
		go func() { res, _ := s.URLTest(ctx); done <- res }()
		synctest.Wait()
		obs["a"].dial = func(context.Context) (net.Conn, error) { return nil, errors.New("later failure") }
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		cancel()
		synctest.Wait()
		if _, ok := (<-done)["a"]; !ok {
			t.Error("URLTest lost a success gathered while waiting for b")
		}
	})
}

type delayedPauseManager struct {
	pause.Manager
	networkPaused atomic.Bool
}

func (p *delayedPauseManager) IsNetworkPaused() bool { return p.networkPaused.Load() }

func TestSchedulerChecksPauseBeforeCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		ctx := pause.WithDefaultManager(s.ctx)
		pm := &delayedPauseManager{Manager: service.FromContext[pause.Manager](ctx)}
		s.ctx = service.ContextWith[pause.Manager](ctx, pm)
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		pm.networkPaused.Store(true)
		close(release)
		synctest.Wait()
		require.Nil(t, s.history.Load("a"))
		require.Equal(t, probeIfStale, s.probeMembers["a"].need)
		pm.networkPaused.Store(false)
		s.onProbePause(pause.EventNetworkWake)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.NotNil(t, s.history.Load("a"))
	})
}

func TestSchedulerURLTestSurvivesNetworkChangeWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		var dials atomic.Int32
		release := make(chan struct{})
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			<-release
			return probeResponderConn(), nil
		}
		done := make(chan map[string]uint16, 1)
		go func() { result, _ := s.URLTest(t.Context()); done <- result }()
		synctest.Wait()
		require.False(t, s.probing)
		s.InterfaceUpdated(context.Background())
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		select {
		case <-done:
			t.Fatal("URLTest ended on a network change")
		default:
		}
		close(release)
		synctest.Wait()
		result := <-done
		require.Contains(t, result, "a")
		require.Contains(t, result, "b")
	})
}

func TestSchedulerURLTestBeforeStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		s.outboundMgr = &mockOutboundManager{outbounds: map[string]A.Outbound{"a": obs["a"]}}
		s.members.Delete("a")
		result, err := s.URLTest(t.Context())
		require.NoError(t, err)
		require.Contains(t, result, "a")
		require.False(t, s.started)
	})
}

func TestSchedulerFilteredNeedDoesNotWeakenForcedNeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newSchedulerTest(t, "a")
		pm := withPauseManager(s)
		pm.NetworkPause()
		requestTestProbes(s, nil, probeForced)
		requestTestProbes(s, nil, probeIfStale)
		require.Equal(t, probeForced, s.probeMembers["a"].need)
		pm.NetworkWake()
		synctest.Wait()
		require.Equal(t, probeNotNeeded, s.probeMembers["a"].need)
	})
}

func TestSchedulerStuckMemberCannotSuppressExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) { <-release; return probeResponderConn(), nil }
		recordSuccess(s, "a", 1)
		done := make(chan struct{})
		go func() { s.runProbeCycle(); close(done) }()
		synctest.Wait()
		time.Sleep(probeBound(behaviorFor(obs["a"].Type())))
		synctest.Wait()
		<-done
		require.True(t, exhausted(s))
		s.Close()
		close(release)
		synctest.Wait()
	})
}

func TestSchedulerLadderRejectsSuccessAfterBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			time.Sleep(1500 * time.Millisecond)
			return probeResponderConn(), nil
		}
		s.access.Lock()
		w := s.newProbeWaiterLocked(false)
		w.deadline = time.Now().Add(time.Second)
		s.requestProbesLocked(nil, probeForced, false)
		s.dispatchProbesLocked()
		s.access.Unlock()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		s.access.Lock()
		s.evaluateProbesLocked(w)
		s.access.Unlock()
		if !exhausted(s) {
			t.Error("success after ladder deadline suppressed exhaustion")
		}
	})
}

func TestSchedulerLadderPreservesEvidenceAtCutoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		obs["b"].dial = func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
		s.access.Lock()
		waiter := s.newProbeWaiterLocked(false)
		s.ladderWaiter = waiter
		waiter.deadline = time.Now().Add(time.Second)
		s.requestProbesLocked(nil, probeForced, false)
		s.dispatchProbesLocked()
		s.access.Unlock()
		synctest.Wait()
		time.Sleep(time.Second)
		obs["a"].dial = denyDial
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		s.access.Lock()
		evaluated := waiter.evaluated
		s.evaluateProbesLocked(waiter)
		s.access.Unlock()
		require.True(t, evaluated)
		require.False(t, exhausted(s))
	})
}

func TestSchedulerRemovalHoldsSlotUntilOperationReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		s.cfg.probeConcurrency = 1
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			<-release
			return nil, errors.New("removed")
		}
		var second atomic.Int32
		obs["b"].dial = func(context.Context) (net.Conn, error) {
			second.Add(1)
			return probeResponderConn(), nil
		}
		requestTestProbes(s, nil, probeForced)
		synctest.Wait()
		require.Zero(t, second.Load())
		n, err := s.Remove("a")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		synctest.Wait()
		require.Zero(t, second.Load(), "the removed member's dial still holds the only slot")
		require.Contains(t, s.inFlightProbes, obs["a"])
		require.Equal(t, 1, s.probeSlotsUsed)
		close(release)
		synctest.Wait()
		require.NotContains(t, s.inFlightProbes, obs["a"])
		require.EqualValues(t, 1, second.Load())
	})
}

func TestSchedulerWatchdogDoesNotRetryAfterIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		s.started = true
		s.cfg.idleThreshold = time.Second
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			dials.Add(1)
			time.Sleep(2 * probeBound(behaviorFor(obs["a"].Type())))
			return nil, errors.New("late failure")
		}
		_, _, err := s.selectOutbound("tcp", "")
		require.NoError(t, err)
		synctest.Wait()
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		require.False(t, s.probing)
		require.EqualValues(t, 1, dials.Load())
		require.EqualValues(t, 1, s.history.Load("a").ConsecutiveFailures)
		require.Empty(t, s.inFlightProbes)
		require.Equal(t, probeNotNeeded, s.probeMembers["a"].need)
	})
}

func TestSchedulerWatchdogPendingRequestsCoalesce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		first, second := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-first
			} else {
				<-second
			}
			return probeResponderConn(), nil
		}
		bound := probeBound(behaviorFor(obs["a"].Type()))
		s.CheckOutbounds()
		synctest.Wait()
		time.Sleep(bound)
		synctest.Wait()
		require.Equal(t, probeNotNeeded, s.probeMembers["a"].need)
		time.Sleep(probeFreshnessWindow)
		for range 10 {
			s.CheckOutbounds()
		}
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		close(first)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		time.Sleep(bound)
		synctest.Wait()
		close(second)
		synctest.Wait()
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.EqualValues(t, 2, s.history.Load("a").ConsecutiveFailures)
		require.Empty(t, s.inFlightProbes)
	})
}

func TestSchedulerWatchdogPreservesNewerConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
				return probeResponderConn(), nil
			}
			return nil, errors.New("confirmation failed")
		}
		recordSuccess(s, "b", 100)
		s.stickyTag.tcp.Store("a")
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		s.requestFailureConfirmation("a", "b", s.networkGeneration.Load())
		time.Sleep(probeBound(behaviorFor(obs["a"].Type())))
		synctest.Wait()
		require.Equal(t, "a", s.Now())
		require.Equal(t, probeForced, s.probeMembers["a"].need)
		close(release)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load())
		require.Equal(t, "b", s.Now())
		require.Equal(t, probeNotNeeded, s.probeMembers["a"].need)
	})
}

func TestSchedulerWatchdogPreservesChangedScope(t *testing.T) {
	for _, change := range []string{"network", "url"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newSchedulerTest(t, "a")
				s.probing = true
				release := make(chan struct{})
				var dials atomic.Int32
				obs["a"].dial = func(context.Context) (net.Conn, error) {
					if dials.Add(1) == 1 {
						<-release
					}
					return probeResponderConn(), nil
				}
				s.CheckOutbounds()
				synctest.Wait()
				if change == "network" {
					s.InterfaceUpdated(context.Background())
				} else {
					s.SetURLOverrides(map[string]string{"a": "http://other.test/"})
				}
				time.Sleep(probeBound(behaviorFor(obs["a"].Type())))
				synctest.Wait()
				require.Equal(t, probeForced, s.probeMembers["a"].need)
				require.EqualValues(t, 1, dials.Load())
				if snapshot := s.history.Load("a"); snapshot != nil {
					require.Zero(t, snapshot.ConsecutiveFailures)
				}
				close(release)
				synctest.Wait()
				require.EqualValues(t, 2, dials.Load())
				require.True(t, s.probeMembers["a"].outcome.success)
				require.Equal(t, probeNotNeeded, s.probeMembers["a"].need)
			})
		})
	}
}

func TestSchedulerRepeatedConfirmationKeepsBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a", "b")
		release := make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			dials.Add(1)
			<-release
			return nil, errors.New("confirmation failed")
		}
		recordSuccess(s, "b", 100)
		s.stickyTag.tcp.Store("a")
		generation := s.networkGeneration.Load()
		s.requestFailureConfirmation("a", "b", generation)
		synctest.Wait()
		for range 5 {
			s.requestFailureConfirmation("a", "", generation)
		}
		synctest.Wait()
		close(release)
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		require.Equal(t, "b", s.Now(), "the running confirmation still satisfies repeated requests")
	})
}

func TestSchedulerRepeatedConfirmationReplacement(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, repeat string
		want          string
	}{
		{"empty keeps pending", "c", "", "c"},
		{"named replaces pending", "c", "d", "d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newSchedulerTest(t, "a", "b", "c", "d")
				release := make(chan struct{})
				obs["a"].dial = func(context.Context) (net.Conn, error) {
					<-release
					return nil, errors.New("confirmation failed")
				}
				recordSuccess(s, "b", 100)
				recordSuccess(s, "c", 200)
				recordSuccess(s, "d", 300)
				s.stickyTag.tcp.Store("a")
				generation := s.networkGeneration.Load()
				s.requestFailureConfirmation("a", tc.first, generation)
				synctest.Wait()
				s.requestFailureConfirmation("a", tc.repeat, generation)
				close(release)
				synctest.Wait()
				require.Equal(t, tc.want, s.Now())
			})
		})
	}
}

func TestSchedulerInterfaceUpdatedRejectsStaleProbeOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		seedProbe(s, "a", true, 12)
		release, hold := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-release
				return nil, errors.New("dial refused")
			}
			<-hold
			return probeResponderConn(), nil
		}
		requestTestProbes(s, []string{"a"}, probeForced)
		synctest.Wait()
		s.InterfaceUpdated(context.Background())
		close(release)
		synctest.Wait()
		require.EqualValues(t, 2, dials.Load(), "the need survives the network change")
		require.Zero(t, s.history.Load("a").ConsecutiveFailures, "a probe from the previous network records nothing")
		require.EqualValues(t, 12, s.history.Load("a").LastSuccessDelayMs)
		close(hold)
		synctest.Wait()
	})
}

func TestSchedulerInterfaceUpdatedRejectsStaleUserFailure(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	s.hist.userFailureDedupeWindow = 0
	s.history.Store("a", &adapter.TagHistory{LastSuccessDelayMs: 12, UpdatedAt: time.Now()})
	s.access.Lock()
	s.hydrateHistoryLocked("a")
	s.access.Unlock()
	generation := s.networkGeneration.Load()
	require.True(t, s.recordUserFailure("a", adapter.UserFailureStall, generation))
	s.InterfaceUpdated(context.Background())
	require.NotEqual(t, generation, s.networkGeneration.Load())
	require.False(t, s.recordUserFailure("a", adapter.UserFailureReset, generation))
	require.Contains(t, s.histories, "a")
	snapshot := s.history.Load("a")
	require.EqualValues(t, 12, snapshot.LastSuccessDelayMs)
	require.Len(t, snapshot.UserFailures, 1)
}

func TestSchedulerInterfaceUpdatedPreservesSelection(t *testing.T) {
	s, _ := newTestMUR(t, "a")
	generation := s.networkGeneration.Load()
	s.stickyTag.tcp.Store("a")
	s.stickyTag.udp.Store("a")
	at := time.Now()
	s.lastConfirmedFailureSwitch.at = at
	s.lastConfirmedFailureSwitch.selectedTag = "a"
	s.lastLadderAt.Store(at.UnixNano())
	s.exhaustionCh <- struct{}{}
	s.InterfaceUpdated(context.Background())
	require.NotEqual(t, generation, s.networkGeneration.Load())
	require.Equal(t, "a", loadString(&s.stickyTag.tcp))
	require.Equal(t, "a", loadString(&s.stickyTag.udp))
	require.Equal(t, at, s.lastConfirmedFailureSwitch.at)
	require.Equal(t, "a", s.lastConfirmedFailureSwitch.selectedTag)
	require.Zero(t, s.lastLadderAt.Load())
	require.Len(t, s.exhaustionCh, 1)
}

func TestSchedulerLastResortReachabilityNeedsFreshSuccess(t *testing.T) {
	tests := []struct {
		name      string
		age       time.Duration
		exhausted bool
	}{
		{"fresh", time.Minute, false},
		{"stale", lastResortSuccessFreshness + time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, obs := newLastResortMUR(t)
				s.defaultURL = "http://probe.test/"
				obs["a"].dial = denyDial
				release := make(chan struct{})
				var dials atomic.Int32
				obs["ub"].dial = func(context.Context) (net.Conn, error) {
					if dials.Add(1) > 1 {
						<-release
					}
					return probeResponderConn(), nil
				}
				s.CheckOutbounds()
				synctest.Wait()
				require.True(t, s.probeMembers["ub"].outcome.success)
				time.Sleep(tt.age)
				s.runProbeCycle()
				synctest.Wait()
				// The cycle's own last-resort probe is still running, so only
				// the earlier success can count.
				require.EqualValues(t, 2, dials.Load())
				require.Equal(t, tt.exhausted, exhausted(s))
				close(release)
				synctest.Wait()
			})
		})
	}
}

func TestSchedulerLastResortURLChangeRejectsOldOutcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		obs["a"].dial = denyDial
		first, second := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		obs["ub"].dial = func(context.Context) (net.Conn, error) {
			if dials.Add(1) == 1 {
				<-first
			} else {
				<-second
			}
			return probeResponderConn(), nil
		}
		s.CheckOutbounds()
		synctest.Wait()
		s.SetURLOverrides(map[string]string{"ub": "http://new.test/"})
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		close(first)
		synctest.Wait()
		require.Nil(t, s.history.Load("ub"))
		require.EqualValues(t, 2, dials.Load())
		close(second)
		synctest.Wait()
		require.Equal(t, "http://new.test/", s.probeMembers["ub"].outcome.url)
		require.Positive(t, s.history.Load("ub").LastSuccessDelayMs)
	})
}

func TestSchedulerInterfaceFlapsDoNotLiftLadderCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newTestMUR(t, "a")
		s.cfg.ladderCooldown = time.Minute
		ladderRan := func() {
			s.lastLadderAt.Store(time.Now().UnixNano())
		}
		ladderRan()
		s.InterfaceUpdated(context.Background())
		require.Zero(t, s.lastLadderAt.Load(), "first update after a quiet period lifts the cooldown")
		for range 3 {
			ladderRan()
			time.Sleep(30 * time.Second)
			s.InterfaceUpdated(context.Background())
			require.NotZero(t, s.lastLadderAt.Load(), "a flap within ladderCooldown keeps the cooldown")
		}
		ladderRan()
		time.Sleep(time.Minute)
		s.InterfaceUpdated(context.Background())
		require.Zero(t, s.lastLadderAt.Load(), "an update after a quiet ladderCooldown lifts it again")
	})
}

func TestSchedulerPausedLadderLeavesCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		pm := withPauseManager(s)
		s.cfg.ladderCooldown = time.Minute
		s.cfg.ladderTotalBudget = time.Minute
		obs["a"].dial = func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		done := make(chan struct{})
		go func() { s.runLadder("a", s.networkGeneration.Load()); close(done) }()
		synctest.Wait()
		pm.DevicePause()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("ladder kept waiting through a pause")
		}
		require.Zero(t, s.lastLadderAt.Load())
		pm.DeviceWake()
	})
}

func TestSchedulerCycleCountsFreshFailureWithoutReprobing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		var dials atomic.Int32
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("dial denied")
		}
		s.CheckOutbounds()
		synctest.Wait()
		require.False(t, s.probeMembers["a"].outcome.success)
		s.runProbeCycle()
		synctest.Wait()
		require.EqualValues(t, 1, dials.Load())
		require.True(t, exhausted(s))
	})
}

func TestSchedulerHardDemotedLastResortDoesNotSuppressExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newLastResortMUR(t)
		s.defaultURL = "http://probe.test/"
		obs["a"].dial = denyDial
		obs["ub"].dial = func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }
		s.CheckOutbounds()
		synctest.Wait()
		require.True(t, s.probeMembers["ub"].outcome.success)

		s.runProbeCycle()
		require.False(t, exhausted(s), "a fresh last-resort success keeps the group reachable")

		addUserFailureN(s, "ub", int(s.hist.consecutiveFailLimit))
		s.runProbeCycle()
		require.True(t, exhausted(s), "a hard-demoted last resort can't carry traffic")
	})
}

func TestSchedulerCycleCountsMemberAddedWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		b := &mockOutbound{tag: "b", dial: func(context.Context) (net.Conn, error) { return probeResponderConn(), nil }}
		s.outboundMgr = &mockOutboundManager{outbounds: map[string]A.Outbound{"b": b}}
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			<-release
			return nil, errors.New("down")
		}
		done := make(chan struct{})
		go func() { s.runProbeCycle(); close(done) }()
		synctest.Wait()
		_, err := s.Add("b")
		require.NoError(t, err)
		synctest.Wait()
		close(release)
		<-done
		require.False(t, exhausted(s), "a member added during the cycle succeeded")
	})
}

func TestSchedulerLadderSkippedByPauseFlagLeavesCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		ctx := pause.WithDefaultManager(s.ctx)
		pm := &delayedPauseManager{Manager: service.FromContext[pause.Manager](ctx)}
		s.ctx = service.ContextWith[pause.Manager](ctx, pm)
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			<-release
			return probeResponderConn(), nil
		}
		done := make(chan struct{})
		go func() { s.runLadder("a", s.networkGeneration.Load()); close(done) }()
		synctest.Wait()
		// Paused, but the callback that advances the pause epoch hasn't run.
		pm.networkPaused.Store(true)
		close(release)
		<-done
		require.Zero(t, s.lastLadderAt.Load(), "a ladder that skipped evaluation sets no cooldown")
	})
}

func TestSchedulerLadderEvaluatedBeforeNetworkChangeLeavesCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, obs := newSchedulerTest(t, "a")
		release := make(chan struct{})
		obs["a"].dial = func(context.Context) (net.Conn, error) {
			<-release
			return nil, errors.New("down")
		}
		done := make(chan struct{})
		go func() { s.runLadder("a", s.networkGeneration.Load()); close(done) }()
		synctest.Wait()
		// The budget timer evaluates the ladder, then the network changes
		// before runLadder resumes.
		s.access.Lock()
		s.evaluateProbesLocked(s.ladderWaiter)
		s.access.Unlock()
		s.InterfaceUpdated(context.Background())
		<-done
		require.Zero(t, s.lastLadderAt.Load(), "a network change lifts the cooldown")
		close(release)
		synctest.Wait()
	})
}
