package group

import (
	"slices"
	"sync"
	"time"

	"github.com/getlantern/lantern-box/adapter"
)

const (
	defaultConsecutiveFailLimit     = 3
	defaultSoftFailLimit            = 2
	defaultUserFailureWindow        = 5 * time.Minute
	defaultUserFailureDedupeWindow  = 30 * time.Second
	defaultDataPlaneIdle            = 60 * time.Second
	defaultDataPlaneProvedReadBytes = 4096
	defaultMaxPersistedAge          = 15 * time.Minute
	// switchPenaltyAltFactor is the ratio at which the best alternative
	// is considered "much slower" than the candidate under evaluation.
	// When best_alt_delay > self_delay * this factor, the demote rule
	// requires twice as many failures before hard-demoting — switching
	// the user from a fast member onto a much slower one should need
	// more evidence than switching between two similar-latency members.
	switchPenaltyAltFactor = 3
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
	userFailures        []adapter.UserFailure
}

func newLocalHistory() *localHistory { return &localHistory{} }

// recordProbeSuccess updates the probe scalars on a successful probe.
// delayMs has already been clamped to ≥1 ms by the caller so the
// "no measurement" sentinel (0) stays unambiguous.
func (h *localHistory) recordProbeSuccess(delayMs uint32, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSuccessDelayMs = delayMs
	h.lastOutcomeAt = now
	h.consecutiveFailures = 0
}

// recordProbeFailure increments the consecutive-failure counter and
// updates lastOutcomeAt. lastSuccessDelayMs is NOT cleared so a member
// with one transient failure still has a real delay to rank by.
func (h *localHistory) recordProbeFailure(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutiveFailures++
	h.lastOutcomeAt = now
}

// addUserFailure appends the failure and prunes entries older than window
// in place. A data-plane stall and a dial error each count as one failure;
// hard demotion requires three in the window.
//
// dedupe collapses bursts to a single failure: if the most recent
// entry is newer than dedupe, the append is dropped. A single broken
// outbound with many idle conns hitting their stall timer in sequence
// would otherwise inflate the count out of proportion to the event.
// The dedupe window spans kinds so a dial error immediately followed by
// a stall on the retry counts once, matching the one-event intent.
// Returns true if the failure was recorded.
func (h *localHistory) addUserFailure(failure adapter.UserFailure, window, dedupe time.Duration) bool {
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

// userFailureCount returns the number of user-traffic failures inside
// the sliding window relative to now. Stale entries are pruned in
// place so the count reflects only the live window.
func (h *localHistory) userFailureCount(now time.Time, window time.Duration) uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, window)
	return uint32(len(h.userFailures))
}

func (h *localHistory) snapshot(now time.Time, window time.Duration) (lastDelay uint32, lastAt time.Time, consec uint32, userFails []adapter.UserFailure) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, window)
	lastDelay = h.lastSuccessDelayMs
	lastAt = h.lastOutcomeAt
	consec = h.consecutiveFailures
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
