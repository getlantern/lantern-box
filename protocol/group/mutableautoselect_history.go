package group

import (
	"maps"
	"slices"
	"sync"
	"time"

	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/adapter"
)

const (
	defaultConsecutiveFailLimit     = 3
	defaultSoftFailLimit            = 2
	defaultUserFailureWindow        = 5 * time.Minute
	defaultUserFailureDedupeWindow  = 45 * time.Second
	defaultDataPlaneIdle            = 60 * time.Second
	defaultDataPlaneProvedReadBytes = 4096
	defaultMaxPersistedAge          = 15 * time.Minute
	// Recovery requires a successful probe after this duration, not just elapsed time.
	recoveryGateMinDuration = 5 * time.Minute
	// switchPenaltyAltFactor is the ratio at which the best alternative
	// is considered "much slower" than the candidate under evaluation.
	// When best_alt_delay > self_delay * this factor, the demote rule
	// requires twice as many failures before hard-demoting — switching
	// the user from a fast member onto a much slower one should need
	// more evidence than switching between two similar-latency members.
	switchPenaltyAltFactor = 3
	// consecutiveUserFailureLimit soft-demotes a member, and releases it as
	// the selection, once its user traffic fails this many times in a row:
	// each failure on a conn opened after the previous one, with no conn
	// opened since then answering.
	consecutiveUserFailureLimit = 2
	// streakSwitchCooldown spaces releases for consecutive failures on one
	// network. Two failures in a row also happen by chance on a member that
	// mostly works, and releasing on each such pair walks the selection
	// across working members; a member that hasn't answered since the last
	// release is released at once, as a broken pick.
	streakSwitchCooldown = 5 * time.Minute
	// udpRetryWindow bounds the gap between the starts of consecutive failed
	// UDP exchanges: an app recovering from a dead relay retries within
	// seconds (a resolver's 5s timeout, WireGuard's 5s handshake retry),
	// while a one-way periodic sender, like a WireGuard client's keepalive
	// that its peer never answers, typically sends every 10s or more on a
	// working relay.
	udpRetryWindow = 7 * time.Second
)

type historyParams struct {
	consecutiveFailLimit    uint32
	softFailLimit           uint32
	userFailureWindow       time.Duration
	userFailureDedupeWindow time.Duration
}

func defaultHistoryParams() historyParams {
	return historyParams{
		consecutiveFailLimit:    defaultConsecutiveFailLimit,
		softFailLimit:           defaultSoftFailLimit,
		userFailureWindow:       defaultUserFailureWindow,
		userFailureDedupeWindow: defaultUserFailureDedupeWindow,
	}
}

// localHistory is the in-memory selection history for one server tag.
//
// The probe-outcome scalars (lastSuccessDelayMs, lastOutcomeAt,
// consecutiveFailures) and the user-traffic userFailures window are
// kept on separate tracks. A probe success never clears userFailures,
// so a censor that lets the probe URL through while dropping the
// user's traffic shows up as healthy probe scalars alongside elevated
// userFailures simultaneously — the anti-laundering signal.
//
// Failures age out of userFailures naturally on the next mutation,
// without depending on traffic being routed through the member, which
// breaks the circular dependency where a hard-demoted candidate would
// otherwise need a successful Read it could never get to recover.
type localHistory struct {
	mu                  sync.Mutex
	lastSuccessDelayMs  uint32
	lastOutcomeAt       time.Time
	consecutiveFailures uint32
	// failureTimes holds when the latest of consecutiveFailures happened,
	// up to maxFailureTimes; failures restored from storage have none.
	// In-memory only.
	failureTimes []time.Time
	userFailures []adapter.UserFailure
	// In-memory only; zero means no recovery gate.
	recoveryGateStartedAt time.Time
	// streaks holds each network's consecutive user-traffic failures.
	// In-memory only.
	streaks [2]failureStreak
	// udpAnswered maps a UDP destination to when a handle to it last
	// answered through this member, keeping the most recent
	// maxUDPAnsweredDestinations. In-memory only.
	udpAnswered map[string]time.Time
	// udpInherited holds the destinations that answered through the UDP
	// selection this member last replaced, so one failing through this
	// member as well is its failure too. They aren't passed on again, which
	// bounds how far a destination that died moves the selection.
	// In-memory only.
	udpInherited map[string]time.Time
}

func newLocalHistory() *localHistory { return &localHistory{} }

func (h *localHistory) recordProbe(success bool, delayMs uint32, now time.Time) {
	if success {
		h.recordProbeSuccess(delayMs, now)
	} else {
		h.recordProbeFailure(now)
	}
}

// recordProbeSuccess updates the probe scalars on a successful probe.
// delayMs has already been clamped to ≥1 ms by the caller so the
// "no measurement" sentinel (0) stays unambiguous.
func (h *localHistory) recordProbeSuccess(delayMs uint32, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSuccessDelayMs = delayMs
	h.lastOutcomeAt = now
	h.consecutiveFailures, h.failureTimes = 0, h.failureTimes[:0]
	if !h.recoveryGateStartedAt.IsZero() && now.Sub(h.recoveryGateStartedAt) >= recoveryGateMinDuration {
		h.recoveryGateStartedAt = time.Time{}
	}
}

// recordProbeFailure increments the consecutive-failure counter and
// updates lastOutcomeAt. lastSuccessDelayMs is NOT cleared so a member
// with one transient failure still has a real delay to rank by.
func (h *localHistory) recordProbeFailure(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failureTimes = append(h.failureTimes, now)
	if len(h.failureTimes) > maxFailureTimes {
		h.failureTimes = h.failureTimes[1:]
	}
	h.consecutiveFailures++
	h.lastOutcomeAt = now
}

// recordForgivenProbeFailure records a failed probe's time without counting
// it, for a failure blamed on the device's own network.
func (h *localHistory) recordForgivenProbeFailure(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastOutcomeAt = now
}

// maxFailureTimes bounds the failure times kept for forgiveness.
const maxFailureTimes = 8

// forgiveProbeFailures uncounts the consecutive probe failures blamed on the
// device's own network: those no earlier than since. Earlier ones stay,
// since its member was failing, or unlucky, before. It reports whether any
// were uncounted.
func (h *localHistory) forgiveProbeFailures(since time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.consecutiveFailures
	kept := h.failureTimes[:0]
	for _, t := range h.failureTimes {
		if t.Before(since) {
			kept = append(kept, t)
		} else if h.consecutiveFailures > 0 {
			h.consecutiveFailures--
		}
	}
	h.failureTimes = kept
	return h.consecutiveFailures != before
}

func (h *localHistory) startRecoveryGate(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recoveryGateStartedAt = now
}

// addUserFailure records the failure of a conn on network opened at opened
// in that network's failure streak, then appends it to the window and prunes
// entries older than window in place. A data-plane stall and a dial error each
// count as one failure; hard demotion requires three in the window. A zero
// opened means the conn's age is unknown.
//
// dedupe collapses bursts to a single failure: if the most recent
// entry is newer than dedupe, the append is dropped. A single broken
// outbound with many idle conns hitting their stall timer in sequence
// would otherwise inflate the count out of proportion to the event.
// The dedupe window spans kinds so a dial error immediately followed by
// a stall on the retry counts once, matching the one-event intent.
// Returns true if the failure was appended.
func (h *localHistory) addUserFailure(failure adapter.UserFailure, network streakNet, opened time.Time, window, dedupe time.Duration) bool {
	h.addUnanswered(network, failure.At, opened, window)
	return h.addWindowFailure(failure, window, dedupe)
}

// addWindowFailure is addUserFailure without the failure streak.
func (h *localHistory) addWindowFailure(failure adapter.UserFailure, window, dedupe time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if dedupe > 0 && len(h.userFailures) > 0 {
		last := h.userFailures[len(h.userFailures)-1]
		if failure.At.Sub(last.At) < dedupe {
			return false
		}
	}
	h.userFailures = pruneUserFailures(append(h.userFailures, failure), failure.At, window)
	return true
}

// addUnanswered records, in network's failure streak only, a conn opened at
// opened that went unanswered. Alone it is as likely a caller abandoning a
// request as a failing path, so it never enters the failure window.
func (h *localHistory) addUnanswered(network streakNet, at, opened time.Time, window time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streaks[network].extend(at, opened, window, network.retryWindow())
}

// noteUserResponse records that a conn on network to dest, opened at opened,
// answered at at. A TCP answer ends the TCP streak only if the conn was
// opened after the streak's latest failure: a path can keep established flows
// alive while failing every new one. Any UDP answer ends the UDP streak, so a
// slow destination's late reply cancels its own unanswered report.
func (h *localHistory) noteUserResponse(network streakNet, dest string, opened, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := &h.streaks[network]
	st.lastAnswerAt = at
	if network == streakUDP || opened.After(st.at) {
		st.count = 0
	}
	if network == streakUDP && dest != "" {
		if h.udpAnswered == nil {
			h.udpAnswered = make(map[string]time.Time)
		}
		if _, ok := h.udpAnswered[dest]; !ok && len(h.udpAnswered) >= maxUDPAnsweredDestinations {
			h.evictOldestUDPAnsweredLocked()
		}
		h.udpAnswered[dest] = at
	}
}

// maxUDPAnsweredDestinations bounds the UDP destinations a member remembers.
const maxUDPAnsweredDestinations = 64

func (h *localHistory) evictOldestUDPAnsweredLocked() {
	var oldest string
	var oldestAt time.Time
	for dest, at := range h.udpAnswered {
		if oldest == "" || at.Before(oldestAt) {
			oldest, oldestAt = dest, at
		}
	}
	delete(h.udpAnswered, oldest)
}

// udpDestAnswered reports whether a UDP handle to dest has answered through
// this member. It has no age limit: a relay that dies after UDP has been
// quiet for a while must still be detectable.
func (h *localHistory) udpDestAnswered(dest string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.udpAnswered[dest]
	_, inherited := h.udpInherited[dest]
	return ok || inherited
}

// inheritUDPDestinations replaces h's inherited destinations with those that
// answered through from itself.
func (h *localHistory) inheritUDPDestinations(from *localHistory) {
	from.mu.Lock()
	dests := maps.Clone(from.udpAnswered)
	from.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.udpInherited = dests
}

func (h *localHistory) streakCount(network streakNet, now time.Time, window time.Duration) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streaks[network].live(now, window)
}

func (h *localHistory) streakCounts(now time.Time, window time.Duration) (counts [2]uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.streaks {
		counts[i] = h.streaks[i].live(now, window)
	}
	return counts
}

func (h *localHistory) clearStreak(network streakNet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streaks[network].count = 0
}

func (h *localHistory) clearStreaks() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.streaks {
		h.streaks[i].count = 0
	}
}

// clearStreakBefore clears network's streak if its latest failure came
// before t.
func (h *localHistory) clearStreakBefore(network streakNet, t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streaks[network].at.Before(t) {
		h.streaks[network].count = 0
	}
}

// limitReachedAt returns when network's streak live at now reached
// consecutiveUserFailureLimit, or zero if it hasn't.
func (h *localHistory) limitReachedAt(network streakNet, now time.Time, window time.Duration) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.streaks[network]
	if st.live(now, window) < consecutiveUserFailureLimit {
		return time.Time{}
	}
	return st.limitAt
}

// streakStarted returns when network's live streak's first failed conn or
// exchange started, or zero without a live streak.
func (h *localHistory) streakStarted(network streakNet, now time.Time, window time.Duration) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streaks[network].live(now, window) == 0 {
		return time.Time{}
	}
	return h.streaks[network].started
}

func (h *localHistory) answeredSince(network streakNet, t time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streaks[network].lastAnswerAt.After(t)
}

// streakNet indexes a member's per-network failure streaks. A member's UDP
// relay can fail while its TCP path works, and the reverse, so evidence on
// one network never moves the other's selection.
type streakNet uint8

const (
	streakTCP streakNet = iota
	streakUDP
)

// retryWindow is how soon after the start of the previous failed exchange
// on network a failed exchange must start to extend the streak, or zero for
// no bound.
func (n streakNet) retryWindow() time.Duration {
	if n == streakUDP {
		return udpRetryWindow
	}
	return 0
}

func streakNetFor(network string) streakNet {
	if network == N.NetworkUDP {
		return streakUDP
	}
	return streakTCP
}

// failureStreak counts one network's consecutive user-traffic failures on a
// member.
type failureStreak struct {
	count uint32
	// at is the latest failure's time and opened its conn's or exchange's
	// start; started is the first failure's conn's or exchange's start, and
	// limitAt when count reached consecutiveUserFailureLimit.
	at, opened, started, limitAt time.Time
	lastAnswerAt                 time.Time
}

// extend counts a failure at at, on a conn opened at opened. A failure
// extends a live streak only if its conn was opened after the streak's latest
// failure, and, with a non-zero retryWindow, within retryWindow of that
// failure's conn opening; one on an older conn is part of the same event, and
// one opened later starts a new streak. A streak ends once window passes
// without a failure.
func (st *failureStreak) extend(at, opened time.Time, window, retryWindow time.Duration) {
	switch {
	case st.live(at, window) == 0:
		st.count, st.started = 1, opened
	case opened.IsZero() || !opened.After(st.at):
		return
	case retryWindow > 0 && opened.Sub(st.opened) > retryWindow:
		st.count, st.started = 1, opened
	default:
		st.count++
	}
	st.at, st.opened = at, opened
	if st.count == consecutiveUserFailureLimit {
		st.limitAt = at
	}
}

func (st *failureStreak) live(now time.Time, window time.Duration) uint32 {
	if st.count == 0 || !ageWithin(now, st.at, window) {
		return 0
	}
	return st.count
}

// userFailureCount returns the number of user-traffic failures inside
// the sliding window relative to now. Stale entries are pruned in
// place so the count reflects only the live window.
func (h *localHistory) userFailureCount(now time.Time, window time.Duration) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, window)
	return uint32(len(h.userFailures))
}

func (h *localHistory) snapshot(now time.Time, window time.Duration) (lastDelay uint32, consec uint32, userFails []adapter.UserFailure, gated bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, window)
	lastDelay = h.lastSuccessDelayMs
	consec = h.consecutiveFailures
	gated = !h.recoveryGateStartedAt.IsZero()
	if len(h.userFailures) > 0 {
		userFails = make([]adapter.UserFailure, len(h.userFailures))
		copy(userFails, h.userFailures)
	}
	return
}

// toTagHistory snapshots the localHistory into adapter.TagHistory form.
// updatedAt is a parameter so a single mutation call uses one
// timestamp across the in-memory entry and the persisted snapshot.
// HardDemoted is the intrinsic tier: selfMs/bestAltMs are zeroed so the
// switch-penalty boost can't apply, classifying the member from its own
// failure history alone.
func (h *localHistory) toTagHistory(updatedAt time.Time, p historyParams) *adapter.TagHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, updatedAt, p.userFailureWindow)
	hard, _, _ := demoted(h.consecutiveFailures, uint32(len(h.userFailures)), 0, 0, p)
	out := &adapter.TagHistory{
		LastSuccessDelayMs:  h.lastSuccessDelayMs,
		LastOutcomeAt:       h.lastOutcomeAt,
		ConsecutiveFailures: h.consecutiveFailures,
		HardDemoted:         hard,
		UpdatedAt:           updatedAt,
	}
	if len(h.userFailures) > 0 {
		out.UserFailures = make([]adapter.UserFailure, len(h.userFailures))
		copy(out.UserFailures, h.userFailures)
	}
	return out
}

// pruneUserFailures returns the subset of failures whose age is within
// window. Ages are clamped to zero to tolerate clock skew between
// cached entries and now. failures must be sorted by At ascending.
func pruneUserFailures(failures []adapter.UserFailure, now time.Time, window time.Duration) []adapter.UserFailure {
	if len(failures) == 0 {
		return nil
	}
	if window <= 0 {
		// window<=0 disables aging — keep what we have.
		return failures
	}
	firstLive := slices.IndexFunc(failures, func(f adapter.UserFailure) bool {
		return ageWithin(now, f.At, window)
	})
	switch firstLive {
	case 0:
		return failures
	case -1:
		return nil
	default:
		copy(failures, failures[firstLive:])
		return failures[:len(failures)-firstLive]
	}
}

// countUserFailuresInWindow returns the number of entries whose age is
// within window without allocating. Use on the read-only rank hot path
// where pruneUserFailures' in-place mutation isn't needed.
func countUserFailuresInWindow(failures []adapter.UserFailure, now time.Time, window time.Duration) uint32 {
	if window <= 0 {
		return uint32(len(failures))
	}
	var n uint32
	for _, f := range failures {
		if ageWithin(now, f.At, window) {
			n++
		}
	}
	return n
}

// ageWithin reports whether t is within window of now. Negative ages
// (cached entries with future timestamps from clock skew) clamp to 0
// so they still count as "in window."
func ageWithin(now, t time.Time, window time.Duration) bool {
	return max(0, now.Sub(t)) < window
}

// hydrateLocalHistory rebuilds a localHistory from a persisted snapshot.
// Caller has already filtered out entries older than maxPersistedAge
// (the cutoff lives in the group, not here, so a future change to the
// cutoff source doesn't require touching hydrate).
func hydrateLocalHistory(t *adapter.TagHistory, now time.Time, window time.Duration) *localHistory {
	h := newLocalHistory()
	if t == nil {
		return h
	}
	h.lastSuccessDelayMs = t.LastSuccessDelayMs
	h.lastOutcomeAt = t.LastOutcomeAt
	h.consecutiveFailures = t.ConsecutiveFailures
	if len(t.UserFailures) > 0 {
		// ensure the slice is sorted by At ascending
		slices.SortFunc(t.UserFailures, func(a, b adapter.UserFailure) int {
			return a.At.Compare(b.At)
		})
		copies := make([]adapter.UserFailure, len(t.UserFailures))
		copy(copies, t.UserFailures)
		h.userFailures = pruneUserFailures(copies, now, window)
	}
	return h
}

// demoted scales the hard-demote threshold by switchPenaltyAltFactor
// when the best alternative is much slower than the candidate's own
// real-seeded delay: hard-demoting a fast member onto a much slower
// one is itself a cost, so it requires more evidence. boosted reports
// whether the scale was applied so callers can log the rescue.
func demoted(consec, userFailsInWindow, selfMs, bestAltMs uint32, p historyParams) (hard, soft, boosted bool) {
	limit := p.consecutiveFailLimit
	if limit > 0 && selfMs > 0 && bestAltMs > selfMs*switchPenaltyAltFactor {
		limit *= 2
		boosted = true
	}
	if limit > 0 && consec >= limit {
		return true, false, boosted
	}
	if limit > 0 && userFailsInWindow >= limit {
		return true, false, boosted
	}
	if p.softFailLimit > 0 && userFailsInWindow >= p.softFailLimit {
		return false, true, boosted
	}
	return false, false, boosted
}
