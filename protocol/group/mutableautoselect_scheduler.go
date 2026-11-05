package group

import (
	"context"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"

	"github.com/getlantern/lantern-box/internal/probe"
)

// Probe scheduling, all under s.access:
//
//   - Callers raise a member's need; dispatchProbesLocked starts at most one
//     attempt per outbound object. Ordinary attempts share probeConcurrency
//     slots; last-resort and failure-confirmation attempts take none. A slot
//     is held until the operation returns or the watchdog fires, even after
//     cancellation, so canceled dials still count against the cap.
//   - An attempt ends by completing, by its watchdog firing at probeBound, or by
//     cancellation (pause, network change for ordinary members, removal, close).
//   - Completion and the watchdog record an outcome only while the attempt is
//     still valid: same membershipID, object, and URL, the same generation for
//     ordinary members, and not paused. A recorded outcome clears the need
//     unless a confirmation requested after the attempt started is pending.
//     Cancellation records nothing and keeps the need.
//   - The object stays busy until the operation returns. Past the watchdog it
//     is stuck and ranks hard-demoted; its return dispatches only a need that
//     is still pending.
//   - probeSequence orders dispatches and outcomes: waiters accept only
//     outcomes, and confirmations only attempts, sequenced after them.

// probeNeed is ordered: a request never lowers a member's pending need.
type probeNeed uint8

const (
	probeNotNeeded probeNeed = iota
	// probeIfStale skips a member whose latest outcome, success or failure, is
	// within probeFreshnessWindow for its current URL and, unless it is a last
	// resort, the current generation.
	probeIfStale
	probeForced
)

type probeOutcome struct {
	sequence   uint64
	success    bool
	delayMs    uint32
	completed  time.Time
	generation uint64
	url        string
	lastResort bool
}

type memberProbeState struct {
	// membershipID changes when a tag is removed and re-added, so attempts and
	// waiters from the earlier membership are rejected.
	membershipID uint64
	need         probeNeed
	// confirmationPending waits for an attempt sequenced after confirmSequence;
	// a failed one may switch TCP selection to replacement.
	confirmationPending bool
	confirmSequence     uint64
	replacement         string
	// outcome is the latest recorded outcome.
	outcome *probeOutcome
}

type probeAttempt struct {
	sequence     uint64
	tag          string
	outbound     A.Outbound
	membershipID uint64
	generation   uint64
	url          string
	deadline     time.Time
	lastResort   bool
	// finalized is set once the attempt records an outcome or is canceled; it
	// records nothing afterwards.
	finalized bool
	// stuck is set when the watchdog fires before the operation returns.
	stuck bool
	// holdsSlot reports whether the attempt still holds a concurrency slot.
	holdsSlot bool
	cancel    context.CancelFunc
	watchdog  *time.Timer
}

type probeWaitMember struct {
	membershipID uint64
	// fresh is the outcome a filtered waiter accepted at creation instead of
	// waiting for a new one.
	fresh *probeOutcome
}

// probeWaiter tracks the ordinary members present when a periodic cycle, ladder,
// or URLTest began. It finishes once each remaining, unstuck member has a
// current outcome sequenced after it, when its deadline passes, or, unless it
// is a URLTest waiter, when the generation or pause epoch changes.
type probeWaiter struct {
	source    string
	evaluated bool
	// counted is set when evaluation counted reachability rather than
	// skipping it because the group closed, paused or changed network.
	counted bool
	// successes is non-nil only for a URLTest waiter, which collects
	// successes and waits across network and pause changes.
	successes  map[string]*probeOutcome
	sequence   uint64
	generation uint64
	pauseEpoch uint64
	members    map[string]probeWaitMember
	// deadline is the ladder budget, set once a slot is available; other
	// waiters have none.
	deadline time.Time
	timer    *time.Timer
}

func (s *MutableAutoSelect) initProbeState() {
	s.probeMembers = make(map[string]*memberProbeState)
	s.inFlightProbes = make(map[A.Outbound]*probeAttempt)
	s.probeChanged = make(chan struct{})
	s.urlWaiters = make(map[*probeWaiter]struct{})
}

// addProbeMemberLocked gives tag probe state unless it already has some, which
// it keeps from an earlier add until Remove.
func (s *MutableAutoSelect) addProbeMemberLocked(tag string) {
	if s.probeMembers[tag] == nil {
		s.nextMembershipID++
		s.probeMembers[tag] = &memberProbeState{membershipID: s.nextMembershipID}
	}
}

func (s *MutableAutoSelect) notifyProbesLocked() {
	if s.probeChanged != nil {
		close(s.probeChanged)
	}
	s.probeChanged = make(chan struct{})
}

func (s *MutableAutoSelect) requestProbesLocked(tags []string, need probeNeed, ordinaryOnly bool) {
	if tags == nil {
		tags = s.tags
	}
	for _, tag := range tags {
		out, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(out.Type())
		if beh.excludeFromPool || (ordinaryOnly && beh.lastResort) {
			continue
		}
		if state := s.probeMembers[tag]; state != nil && need > state.need {
			state.need = need
		}
	}
}

func (s *MutableAutoSelect) hasFreshProbeOutcomeLocked(tag string, now time.Time) bool {
	state := s.probeMembers[tag]
	if state == nil || state.outcome == nil {
		return false
	}
	return now.Sub(state.outcome.completed) < probeFreshnessWindow && s.outcomeCurrentLocked(tag, state.outcome)
}

// scopeCurrentLocked reports whether a probe of tag against url in generation
// still applies: the member's URL is unchanged and, for an ordinary member,
// so is the network generation. Last-resort results span networks.
func (s *MutableAutoSelect) scopeCurrentLocked(tag, url string, generation uint64, lastResort bool) bool {
	return url == s.probeURLForLocked(tag) && (lastResort || s.matchesGenerationLocked(generation))
}

func (s *MutableAutoSelect) outcomeCurrentLocked(tag string, outcome *probeOutcome) bool {
	return s.scopeCurrentLocked(tag, outcome.url, outcome.generation, outcome.lastResort)
}

func (s *MutableAutoSelect) sameMembershipLocked(tag string, membershipID uint64) bool {
	state := s.probeMembers[tag]
	return state != nil && state.membershipID == membershipID
}

func (s *MutableAutoSelect) probeLimit() int { return s.cfg.probeConcurrency }

func (s *MutableAutoSelect) dispatchProbesLocked() {
	if s.isClosed() || s.paused() {
		return
	}
	s.startLadderBudgetLocked()
	now := time.Now()
	for _, tag := range s.tags {
		state := s.probeMembers[tag]
		if state == nil || state.need == probeNotNeeded {
			continue
		}
		out, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(out.Type())
		if beh.excludeFromPool {
			state.need = probeNotNeeded
			continue
		}
		if state.need == probeIfStale && s.hasFreshProbeOutcomeLocked(tag, now) {
			state.need = probeNotNeeded
			continue
		}
		// A confirmation decides whether to move traffic off a failing
		// selection, so it doesn't wait behind background probes for a slot.
		needsSlot := !beh.lastResort && !state.confirmationPending
		if s.inFlightProbes[out] != nil || (needsSlot && s.probeSlotsUsed >= s.probeLimit()) {
			continue
		}
		s.startProbeLocked(tag, out, beh, state, needsSlot)
	}
}

func (s *MutableAutoSelect) startProbeLocked(
	tag string,
	out A.Outbound,
	beh protocolBehavior,
	state *memberProbeState,
	holdsSlot bool,
) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.probeSequence++
	attempt := &probeAttempt{
		sequence:     s.probeSequence,
		tag:          tag,
		outbound:     out,
		membershipID: state.membershipID,
		generation:   s.networkGeneration.Load(),
		url:          s.probeURLForLocked(tag),
		deadline:     time.Now().Add(probeBound(beh)),
		lastResort:   beh.lastResort,
		holdsSlot:    holdsSlot,
		cancel:       cancel,
	}
	s.inFlightProbes[out] = attempt
	if holdsSlot {
		s.probeSlotsUsed++
	}
	attempt.watchdog = time.AfterFunc(time.Until(attempt.deadline), func() {
		s.access.Lock()
		defer s.access.Unlock()
		if s.inFlightProbes[out] != attempt {
			return
		}
		s.timeoutProbeLocked(attempt)
		s.dispatchProbesLocked()
	})
	go func() {
		delay, err := probe.Run(ctx, out, attempt.url, beh.probeTimeout)
		s.completeProbe(attempt, err == nil, uint32(max(1, delay/time.Millisecond)))
	}()
}

func (s *MutableAutoSelect) validProbeLocked(attempt *probeAttempt) bool {
	if s.isClosed() || s.paused() {
		return false
	}
	out, ok := s.members.Load(attempt.tag)
	return ok && out == attempt.outbound && s.sameMembershipLocked(attempt.tag, attempt.membershipID) &&
		s.scopeCurrentLocked(attempt.tag, attempt.url, attempt.generation, attempt.lastResort)
}

func (s *MutableAutoSelect) releaseProbeSlotLocked(attempt *probeAttempt) {
	if attempt.holdsSlot {
		attempt.holdsSlot = false
		s.probeSlotsUsed--
	}
	s.startLadderBudgetLocked()
}

func (s *MutableAutoSelect) timeoutProbeLocked(attempt *probeAttempt) {
	s.expireLadderLocked()
	if attempt.stuck {
		return
	}
	attempt.stuck = true
	s.logger.Warn("probe watchdog expired: ", attempt.tag)
	s.finishProbeLocked(attempt, false, 0)
	s.notifyProbesLocked()
}

func (s *MutableAutoSelect) completeProbe(attempt *probeAttempt, success bool, delayMs uint32) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.inFlightProbes[attempt.outbound] != attempt {
		return
	}
	if !time.Now().Before(attempt.deadline) {
		s.timeoutProbeLocked(attempt)
	}
	s.finishProbeLocked(attempt, success, delayMs)
	attempt.watchdog.Stop()
	delete(s.inFlightProbes, attempt.outbound)
	s.notifyProbesLocked()
	s.dispatchProbesLocked()
}

func (s *MutableAutoSelect) acceptProbeLocked(attempt *probeAttempt, success bool, delayMs uint32) {
	s.expireLadderLocked()
	if !s.validProbeLocked(attempt) {
		return
	}
	state := s.probeMembers[attempt.tag]
	outcome := s.recordProbeOutcomeLocked(attempt.tag, &probeOutcome{
		success:    success,
		delayMs:    delayMs,
		generation: attempt.generation,
		url:        attempt.url,
		lastResort: attempt.lastResort,
	})
	if success && !attempt.lastResort {
		for waiter := range s.urlWaiters {
			member, ok := waiter.members[attempt.tag]
			if ok && member.membershipID == attempt.membershipID && outcome.sequence > waiter.sequence {
				waiter.successes[attempt.tag] = outcome
			}
		}
	}
	confirmation := state.confirmationPending && attempt.sequence > state.confirmSequence
	if !state.confirmationPending || confirmation {
		state.need = probeNotNeeded
	}
	if confirmation {
		replacement := state.replacement
		state.confirmationPending = false
		state.replacement = ""
		if !success {
			s.trySwitchAfterConfirmedFailureLocked(attempt.tag, replacement)
		}
	}
	s.notifyProbesLocked()
}

// recordProbeOutcomeLocked sequences outcome, makes it tag's latest, and
// records it in tag's history. tag must have probe state.
func (s *MutableAutoSelect) recordProbeOutcomeLocked(tag string, outcome *probeOutcome) *probeOutcome {
	s.probeSequence++
	outcome.sequence = s.probeSequence
	outcome.completed = time.Now()
	s.probeMembers[tag].outcome = outcome
	s.mutateHistoryLocked(tag, func(h *localHistory, now time.Time) bool {
		h.recordProbe(outcome.success, outcome.delayMs, now)
		return true
	})
	return outcome
}

// finishProbeLocked records attempt's outcome unless it already ended, then
// cancels it and frees its slot. Call it once the operation returns or the
// watchdog fires; the attempt stays in inFlightProbes until the operation
// returns.
func (s *MutableAutoSelect) finishProbeLocked(attempt *probeAttempt, success bool, delayMs uint32) {
	if !attempt.finalized {
		attempt.finalized = true
		s.acceptProbeLocked(attempt, success, delayMs)
	}
	attempt.cancel()
	s.releaseProbeSlotLocked(attempt)
}

// cancelProbeLocked ends attempt without recording an outcome. It keeps its
// slot until the operation returns or the watchdog fires.
func (s *MutableAutoSelect) cancelProbeLocked(attempt *probeAttempt) {
	attempt.finalized = true
	attempt.cancel()
}

func (s *MutableAutoSelect) cancelProbesLocked(ordinaryOnly bool) {
	for _, attempt := range s.inFlightProbes {
		if attempt.finalized || (ordinaryOnly && attempt.lastResort) {
			continue
		}
		s.cancelProbeLocked(attempt)
	}
	s.notifyProbesLocked()
}

func (s *MutableAutoSelect) stuckProbeLocked(out A.Outbound) bool {
	attempt := s.inFlightProbes[out]
	return attempt != nil && attempt.stuck
}

func (s *MutableAutoSelect) newProbeWaiterLocked(filtered bool) *probeWaiter {
	now := time.Now()
	waiter := &probeWaiter{
		sequence:   s.probeSequence,
		generation: s.networkGeneration.Load(),
		pauseEpoch: s.pauseEpoch,
		members:    make(map[string]probeWaitMember),
	}
	for _, tag := range s.tags {
		out, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(out.Type())
		state := s.probeMembers[tag]
		if beh.excludeFromPool || beh.lastResort || state == nil {
			continue
		}
		member := probeWaitMember{membershipID: state.membershipID}
		if filtered && s.hasFreshProbeOutcomeLocked(tag, now) {
			member.fresh = state.outcome
		}
		waiter.members[tag] = member
	}
	return waiter
}

func (s *MutableAutoSelect) waiterOutcomeLocked(waiter *probeWaiter, tag string, member probeWaitMember) *probeOutcome {
	if !s.sameMembershipLocked(tag, member.membershipID) {
		return nil
	}
	result := s.probeMembers[tag].outcome
	if result == nil || !s.outcomeCurrentLocked(tag, result) {
		return nil
	}
	if result == member.fresh || result.sequence > waiter.sequence {
		return result
	}
	return nil
}

func (s *MutableAutoSelect) waiterFinishedLocked(waiter *probeWaiter) bool {
	for tag, member := range waiter.members {
		out, ok := s.members.Load(tag)
		if !ok || !s.sameMembershipLocked(tag, member.membershipID) || s.stuckProbeLocked(out) {
			continue
		}
		if s.waiterOutcomeLocked(waiter, tag, member) == nil {
			return false
		}
	}
	return true
}

func (s *MutableAutoSelect) waiterDoneLocked(waiter *probeWaiter) bool {
	switch {
	case s.isClosed():
		return true
	case waiter.successes == nil && s.waiterScopeChangedLocked(waiter):
		return true
	case waiter.pastDeadline(time.Now()):
		return true
	default:
		return s.waiterFinishedLocked(waiter)
	}
}

func (s *MutableAutoSelect) waiterScopeChangedLocked(waiter *probeWaiter) bool {
	return !s.matchesGenerationLocked(waiter.generation) || waiter.pauseEpoch != s.pauseEpoch
}

func (w *probeWaiter) pastDeadline(now time.Time) bool {
	return !w.deadline.IsZero() && !now.Before(w.deadline)
}

func (w *probeWaiter) successWithinBudget(outcome *probeOutcome) bool {
	return outcome.success && (w.deadline.IsZero() || !outcome.completed.After(w.deadline))
}

func (s *MutableAutoSelect) waitForProbes(ctx context.Context, waiter *probeWaiter) {
	for {
		s.access.Lock()
		finished := s.waiterDoneLocked(waiter)
		changed := s.probeChanged
		s.access.Unlock()
		if finished {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-changed:
		}
	}
}

func (s *MutableAutoSelect) evaluateProbesLocked(waiter *probeWaiter) {
	if waiter.evaluated {
		return
	}
	waiter.evaluated = true
	if s.isClosed() || s.paused() || s.waiterScopeChangedLocked(waiter) {
		return
	}
	waiter.counted = true
	reachable := 0
	// The waiter's members decide when to stop waiting, but any current
	// member counts, so one added or re-added meanwhile isn't overlooked.
	for _, tag := range s.tags {
		out, ok := s.members.Load(tag)
		state := s.probeMembers[tag]
		if !ok || state == nil || state.outcome == nil || behaviorFor(out.Type()).lastResort || s.stuckProbeLocked(out) {
			continue
		}
		outcome := state.outcome
		member, inSnapshot := waiter.members[tag]
		acceptedFresh := inSnapshot && member.membershipID == state.membershipID && outcome == member.fresh
		if (acceptedFresh || outcome.sequence > waiter.sequence) && s.outcomeCurrentLocked(tag, outcome) && waiter.successWithinBudget(outcome) {
			reachable++
		}
	}
	// A hard-demoted or stuck last resort ranks demoteHard, and its probe
	// success doesn't make it reachable.
	for _, c := range s.rankLocked(time.Now()) {
		if c.demote != demoteLastResort {
			continue
		}
		state := s.probeMembers[c.tag]
		if state == nil || state.outcome == nil {
			continue
		}
		outcome := state.outcome
		current := s.outcomeCurrentLocked(c.tag, outcome) && time.Since(outcome.completed) <= lastResortSuccessFreshness
		if current && waiter.successWithinBudget(outcome) {
			reachable++
		}
	}
	if waiter.source == "ladder" {
		s.logger.Info("ladder complete: reachable=", reachable)
	} else {
		s.logger.Debug(waiter.source, " complete: reachable=", reachable)
	}
	if reachable == 0 {
		s.logger.Warn(waiter.source, " exhausted: no candidate succeeded")
		s.emitExhaustionLocked()
	}
}

func (s *MutableAutoSelect) startLadderBudgetLocked() {
	waiter := s.ladderWaiter
	if waiter == nil || !waiter.deadline.IsZero() || s.probeSlotsUsed >= s.probeLimit() {
		return
	}
	waiter.deadline = time.Now().Add(s.cfg.ladderTotalBudget)
	waiter.timer = time.AfterFunc(time.Until(waiter.deadline), func() {
		s.access.Lock()
		defer s.access.Unlock()
		s.evaluateProbesLocked(waiter)
		s.notifyProbesLocked()
	})
}

func (s *MutableAutoSelect) runLadder(target string, generation uint64) {
	s.access.Lock()
	if s.isClosed() || s.paused() || s.ladderWaiter != nil || !s.matchesGenerationLocked(generation) {
		s.access.Unlock()
		return
	}
	if last := s.lastLadderAt.Load(); last != 0 && time.Since(time.Unix(0, last)) < s.cfg.ladderCooldown {
		s.access.Unlock()
		return
	}
	waiter := s.newProbeWaiterLocked(false)
	waiter.source = "ladder"
	s.logger.Info("ladder started: ", target)
	s.ladderWaiter = waiter
	s.requestProbesLocked(nil, probeForced, false)
	s.dispatchProbesLocked()
	s.access.Unlock()
	s.waitForProbes(s.ctx, waiter)
	s.access.Lock()
	defer s.access.Unlock()
	if waiter.timer != nil {
		waiter.timer.Stop()
	}
	s.evaluateProbesLocked(waiter)
	// A ladder that skipped evaluation, or whose network has since changed,
	// mustn't hold off the next one.
	if waiter.counted && !s.waiterScopeChangedLocked(waiter) {
		s.lastLadderAt.Store(time.Now().UnixNano())
	}
	s.ladderWaiter = nil
}

func (s *MutableAutoSelect) runProbeCycle() {
	s.access.Lock()
	if s.isClosed() || s.paused() || s.periodicWaiter != nil {
		s.access.Unlock()
		return
	}
	waiter := s.newProbeWaiterLocked(true)
	waiter.source = "cycle"
	s.periodicWaiter = waiter
	s.requestProbesLocked(nil, probeIfStale, false)
	s.dispatchProbesLocked()
	s.access.Unlock()
	s.waitForProbes(s.ctx, waiter)
	s.access.Lock()
	defer s.access.Unlock()
	s.evaluateProbesLocked(waiter)
	s.periodicWaiter = nil
}

// CheckOutbounds requests probes without waiting for results, skipping fresh outcomes.
func (s *MutableAutoSelect) CheckOutbounds() {
	s.access.Lock()
	defer s.access.Unlock()
	s.requestProbesLocked(nil, probeIfStale, false)
	s.dispatchProbesLocked()
}

// URLTest returns ordinary-member successes, including partial results when its context ends.
func (s *MutableAutoSelect) URLTest(ctx context.Context) (map[string]uint16, error) {
	s.access.Lock()
	for _, tag := range s.tags {
		if _, ok := s.members.Load(tag); ok || s.outboundMgr == nil {
			continue
		}
		if out, ok := s.outboundMgr.Outbound(tag); ok {
			s.addMemberLocked(tag, out)
		}
	}
	waiter := s.newProbeWaiterLocked(false)
	waiter.successes = make(map[string]*probeOutcome)
	s.urlWaiters[waiter] = struct{}{}
	s.requestProbesLocked(nil, probeForced, false)
	s.dispatchProbesLocked()
	s.access.Unlock()
	s.waitForProbes(ctx, waiter)
	s.access.Lock()
	defer s.access.Unlock()
	delete(s.urlWaiters, waiter)
	results := make(map[string]uint16)
	for tag, outcome := range waiter.successes {
		if s.sameMembershipLocked(tag, waiter.members[tag].membershipID) && s.outcomeCurrentLocked(tag, outcome) {
			results[tag] = uint16(min(65535, outcome.delayMs))
		}
	}
	return results, nil
}

func (s *MutableAutoSelect) requestFailureConfirmation(tag, replacement string, generation uint64) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() || s.paused() || !s.matchesGenerationLocked(generation) || loadString(&s.stickyTag.tcp) != tag {
		return
	}
	out, ok := s.members.Load(tag)
	if !ok || behaviorFor(out.Type()).excludeFromPool || probe.ValidateURL(s.probeURLForLocked(tag)) != nil {
		return
	}
	s.requestProbesLocked([]string{tag}, probeForced, false)
	state := s.probeMembers[tag]
	// A repeat request keeps the pending boundary, or repeated failures could
	// keep a running confirmation from ever satisfying it.
	if !state.confirmationPending {
		state.confirmationPending = true
		s.probeSequence++
		state.confirmSequence = s.probeSequence
		state.replacement = replacement
	} else if replacement != "" {
		state.replacement = replacement
	}
	s.dispatchProbesLocked()
}

func (s *MutableAutoSelect) registerProbePauseCallback() {
	s.pauseCallbackAccess.Lock()
	defer s.pauseCallbackAccess.Unlock()
	if s.pauseCallback != nil || s.isClosed() {
		return
	}
	pm := service.FromContext[pause.Manager](s.ctx)
	if pm == nil {
		return
	}
	s.probePauseManager = pm
	s.pauseCallback = pm.RegisterCallback(s.onProbePause)
}

// unregisterProbePauseCallback must not be called with s.access held: the
// pause manager holds its own lock while delivering onProbePause.
func (s *MutableAutoSelect) unregisterProbePauseCallback() {
	s.pauseCallbackAccess.Lock()
	defer s.pauseCallbackAccess.Unlock()
	if s.pauseCallback != nil {
		s.probePauseManager.UnregisterCallback(s.pauseCallback)
		s.pauseCallback = nil
	}
}

func (s *MutableAutoSelect) onProbePause(event int) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return
	}
	switch event {
	case pause.EventDevicePaused, pause.EventNetworkPause:
		s.pauseEpoch++
		s.cancelProbesLocked(false)
		if s.periodicTimer != nil {
			s.periodicTimer.Stop()
			s.periodicTimer = nil
		}
	case pause.EventDeviceWake, pause.EventNetworkWake:
		if !s.paused() {
			s.armPeriodicLocked()
			s.dispatchProbesLocked()
		}
	}
}

func (s *MutableAutoSelect) armPeriodicLocked() {
	if !s.probing || s.paused() || s.isClosed() {
		return
	}
	if s.periodicTimer != nil {
		s.periodicTimer.Stop()
	}
	s.periodicTimer = time.AfterFunc(s.cfg.activeInterval, func() {
		s.access.Lock()
		if !s.probing || s.paused() || s.isClosed() {
			s.access.Unlock()
			return
		}
		s.armPeriodicLocked()
		s.access.Unlock()
		s.runProbeCycle()
	})
}

func (s *MutableAutoSelect) keepAliveLocked() {
	if !s.started || s.isClosed() {
		return
	}
	s.lastActive = time.Now()
	if s.probing {
		return
	}
	s.probing = true
	s.logger.Info("background probing started")
	s.requestProbesLocked(nil, probeIfStale, false)
	s.dispatchProbesLocked()
	s.armPeriodicLocked()
	s.armIdleLocked(s.cfg.idleThreshold)
}

func (s *MutableAutoSelect) armIdleLocked(delay time.Duration) {
	s.idleTimer = time.AfterFunc(delay, func() {
		s.access.Lock()
		defer s.access.Unlock()
		if s.isClosed() || !s.probing {
			return
		}
		lastUse := s.lastActive
		if data := time.Unix(0, s.lastDataAt.Load()); data.After(lastUse) {
			lastUse = data
		}
		if remaining := s.cfg.idleThreshold - time.Since(lastUse); remaining > 0 {
			s.armIdleLocked(remaining)
			return
		}
		s.probing = false
		s.logger.Info("background probing stopped: idle")
		if s.periodicTimer != nil {
			s.periodicTimer.Stop()
			s.periodicTimer = nil
		}
	})
}

func (s *MutableAutoSelect) closeProbesLocked() {
	s.cancelProbesLocked(false)
	for _, attempt := range s.inFlightProbes {
		attempt.watchdog.Stop()
	}
	if s.periodicTimer != nil {
		s.periodicTimer.Stop()
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	if s.ladderWaiter != nil && s.ladderWaiter.timer != nil {
		s.ladderWaiter.timer.Stop()
	}
	s.notifyProbesLocked()
}

func (s *MutableAutoSelect) expireLadderLocked() {
	if waiter := s.ladderWaiter; waiter != nil && waiter.pastDeadline(time.Now()) {
		s.evaluateProbesLocked(waiter)
	}
}

func (s *MutableAutoSelect) matchesGenerationLocked(generation uint64) bool {
	return generation == s.networkGeneration.Load()
}

func (s *MutableAutoSelect) advanceNetworkLocked() {
	s.networkGeneration.Add(1)
	now := time.Now()
	// Only the first update after a quiet ladderCooldown lifts the ladder
	// cooldown, so a flapping interface can't rerun the ladder on every flap.
	if now.Sub(s.lastNetworkAdvanceAt) >= s.cfg.ladderCooldown {
		s.lastLadderAt.Store(0)
	}
	s.lastNetworkAdvanceAt = now
	s.cancelProbesLocked(true)
	for _, state := range s.probeMembers {
		state.confirmationPending = false
	}
	if s.probing {
		s.requestProbesLocked(nil, probeForced, true)
	}
	var requested []string
	for waiter := range s.urlWaiters {
		for tag, member := range waiter.members {
			if s.sameMembershipLocked(tag, member.membershipID) {
				requested = append(requested, tag)
			}
		}
	}
	if len(requested) > 0 {
		s.requestProbesLocked(requested, probeForced, true)
	}
}
