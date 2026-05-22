package group

import (
	"sync"
	"time"

	"github.com/getlantern/lantern-box/adapter"
)

const (
	defaultConsecutiveFailLimit    = 3
	defaultSoftFailLimit           = 2
	defaultUserFailureWindow       = 5 * time.Minute
	defaultUserFailureDedupeWindow = 30 * time.Second
	defaultDataPlaneIdle           = 60 * time.Second
	defaultDataPlaneProvedReadBytes = 4096
	defaultMaxPersistedAge         = 15 * time.Minute
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
	userFailures        []time.Time
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

// addUserFailure appends a user-traffic failure timestamp and prunes
// entries older than window in place. A data-plane stall and a dial
// error each count as one failure; hard demotion requires three in
// the window.
//
// dedupe collapses bursts to a single failure: if the most recent
// entry is newer than dedupe, the append is dropped. A single broken
// outbound with many idle conns hitting their stall timer in sequence
// would otherwise inflate the count out of proportion to the event.
// Returns true if the failure was recorded.
func (h *localHistory) addUserFailure(now time.Time, window, dedupe time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if dedupe > 0 && len(h.userFailures) > 0 {
		last := h.userFailures[len(h.userFailures)-1]
		if now.Sub(last) < dedupe {
			return false
		}
	}
	h.userFailures = pruneUserFailures(append(h.userFailures, now), now, window)
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

func (h *localHistory) snapshot(now time.Time, window time.Duration) (lastDelay uint32, lastAt time.Time, consec uint32, userFails []time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, window)
	lastDelay = h.lastSuccessDelayMs
	lastAt = h.lastOutcomeAt
	consec = h.consecutiveFailures
	if len(h.userFailures) > 0 {
		userFails = make([]time.Time, len(h.userFailures))
		copy(userFails, h.userFailures)
	}
	return
}

// toTagHistory snapshots the localHistory into adapter.TagHistory form.
// updatedAt is a parameter so a single mutation call uses one
// timestamp across the in-memory entry and the persisted snapshot.
func (h *localHistory) toTagHistory(updatedAt time.Time, window time.Duration) *adapter.TagHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, updatedAt, window)
	out := &adapter.TagHistory{
		LastSuccessDelayMs:  h.lastSuccessDelayMs,
		LastOutcomeAt:       h.lastOutcomeAt,
		ConsecutiveFailures: h.consecutiveFailures,
		UpdatedAt:           updatedAt,
	}
	if len(h.userFailures) > 0 {
		out.UserFailures = make([]time.Time, len(h.userFailures))
		copy(out.UserFailures, h.userFailures)
	}
	return out
}

// pruneUserFailures returns the subset of failures whose age is within
// window. Ages are clamped to zero to tolerate clock skew between
// cached entries and now.
func pruneUserFailures(failures []time.Time, now time.Time, window time.Duration) []time.Time {
	if len(failures) == 0 {
		return nil
	}
	if window <= 0 {
		// window<=0 disables aging — keep what we have.
		return failures
	}
	out := failures[:0]
	for _, t := range failures {
		if ageWithin(now, t, window) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// countUserFailuresInWindow returns the number of entries whose age is
// within window without allocating. Use on the read-only rank hot path
// where pruneUserFailures' in-place mutation isn't needed.
func countUserFailuresInWindow(failures []time.Time, now time.Time, window time.Duration) uint32 {
	if window <= 0 {
		return uint32(len(failures))
	}
	var n uint32
	for _, t := range failures {
		if ageWithin(now, t, window) {
			n++
		}
	}
	return n
}

// ageWithin reports whether t is within window of now. Negative ages
// (cached entries with future timestamps from clock skew) clamp to 0
// so they still count as "in window."
func ageWithin(now, t time.Time, window time.Duration) bool {
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	return age < window
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
		copies := make([]time.Time, len(t.UserFailures))
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
