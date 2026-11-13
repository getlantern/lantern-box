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
	defaultUserFailureDedupeWindow  = 45 * time.Second
	defaultDataPlaneIdle            = 60 * time.Second
	defaultDataPlaneProvedReadBytes = 4096
	defaultMaxPersistedAge          = 15 * time.Minute
	// maxNetworksPerMember bounds the networks a member keeps history for:
	// the few a device moves between (home and work Wi-Fi, cellular) plus one.
	maxNetworksPerMember = 4
	// Recovery requires a successful probe after this duration, not just elapsed time.
	recoveryGateMinDuration = 5 * time.Minute
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
	lastSuccessAt       time.Time
	lastOutcomeAt       time.Time
	consecutiveFailures uint32
	userFailures        []adapter.UserFailure
	// In-memory only; zero means no recovery gate.
	recoveryGateStartedAt time.Time
	// updatedAt is the time of the last recorded change; it ages the entry
	// out once the group has left this network. Guarded by the group's
	// access lock rather than mu.
	updatedAt time.Time
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
	h.lastSuccessAt = now
	h.lastOutcomeAt = now
	h.consecutiveFailures = 0
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
	h.consecutiveFailures++
	h.lastOutcomeAt = now
}

func (h *localHistory) startRecoveryGate(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recoveryGateStartedAt = now
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

// toNetworkHistory snapshots the localHistory into adapter.NetworkHistory
// form. HardDemoted is the intrinsic tier: selfMs/bestAltMs are zeroed so
// the switch-penalty boost can't apply, classifying the member from its own
// failure history alone.
func (h *localHistory) toNetworkHistory(now time.Time, p historyParams) *adapter.NetworkHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.userFailures = pruneUserFailures(h.userFailures, now, p.userFailureWindow)
	hard, _, _ := demoted(h.consecutiveFailures, uint32(len(h.userFailures)), 0, 0, p)
	out := &adapter.NetworkHistory{
		LastSuccessDelayMs:  h.lastSuccessDelayMs,
		LastSuccessAt:       h.lastSuccessAt,
		LastOutcomeAt:       h.lastOutcomeAt,
		ConsecutiveFailures: h.consecutiveFailures,
		HardDemoted:         hard,
		UpdatedAt:           h.updatedAt,
	}
	if len(h.userFailures) > 0 {
		out.UserFailures = make([]adapter.UserFailure, len(h.userFailures))
		copy(out.UserFailures, h.userFailures)
	}
	return out
}

// lastSuccess returns the last successful probe delay and when it was
// measured.
func (h *localHistory) lastSuccess() (uint32, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastSuccessDelayMs, h.lastSuccessAt
}

// memberHistory is one member's history on each network environment it has
// been used on. Guarded by the group's access lock; each localHistory also
// has its own mutex.
type memberHistory struct {
	byNetwork map[uint64]*localHistory
}

func newMemberHistory() *memberHistory {
	return &memberHistory{byNetwork: make(map[uint64]*localHistory)}
}

// forNetwork returns env's history, creating an empty one if needed.
func (m *memberHistory) forNetwork(env uint64) *localHistory {
	h, ok := m.byNetwork[env]
	if !ok {
		h = newLocalHistory()
		m.byNetwork[env] = h
	}
	return h
}

func (m *memberHistory) peek(env uint64) (*localHistory, bool) {
	h, ok := m.byNetwork[env]
	return h, ok
}

// delayHint returns the most recently observed successful probe delay on any
// network other than exclude that changed within maxAge of now (maxAge <= 0
// disables aging), or 0 if there is none. It carries a latency estimate onto
// a network the member has no history for, never its failures.
func (m *memberHistory) delayHint(exclude uint64, now time.Time, maxAge time.Duration) uint32 {
	var (
		best   uint32
		bestAt time.Time
	)
	for env, h := range m.byNetwork {
		if env == exclude || expired(h.updatedAt, now, maxAge) {
			continue
		}
		if delay, at := h.lastSuccess(); delay > 0 && (best == 0 || at.After(bestAt)) {
			best, bestAt = delay, at
		}
	}
	return best
}

// prune drops every network not in keep whose history was last changed more
// than maxAge before now (maxAge <= 0 disables aging), then the least recently
// changed others until at most maxNetworksPerMember remain.
func (m *memberHistory) prune(now time.Time, maxAge time.Duration, keep ...uint64) {
	for env, h := range m.byNetwork {
		if !slices.Contains(keep, env) && expired(h.updatedAt, now, maxAge) {
			delete(m.byNetwork, env)
		}
	}
	for len(m.byNetwork) > maxNetworksPerMember {
		var (
			oldest   uint64
			oldestAt time.Time
			found    bool
		)
		for env, h := range m.byNetwork {
			if !slices.Contains(keep, env) && (!found || h.updatedAt.Before(oldestAt)) {
				oldest, oldestAt, found = env, h.updatedAt, true
			}
		}
		if !found {
			return
		}
		delete(m.byNetwork, oldest)
	}
}

// expired reports whether history last changed at updatedAt is more than
// maxAge old at now. maxAge <= 0 never expires.
func expired(updatedAt, now time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && now.Sub(updatedAt) > maxAge
}

// toTagHistory snapshots every network's history as of now, with the
// top-level fields, UpdatedAt included, taken from current.
func (m *memberHistory) toTagHistory(current uint64, now time.Time, p historyParams) *adapter.TagHistory {
	out := &adapter.TagHistory{
		Networks: make(map[uint64]*adapter.NetworkHistory, len(m.byNetwork)),
	}
	for env, h := range m.byNetwork {
		n := h.toNetworkHistory(now, p)
		out.Networks[env] = n
		if env == current {
			out.UpdatedAt = n.UpdatedAt
			out.LastSuccessDelayMs = n.LastSuccessDelayMs
			out.LastOutcomeAt = n.LastOutcomeAt
			out.ConsecutiveFailures = n.ConsecutiveFailures
			out.HardDemoted = n.HardDemoted
			if len(n.UserFailures) > 0 {
				out.UserFailures = make([]adapter.UserFailure, len(n.UserFailures))
				copy(out.UserFailures, n.UserFailures)
			}
		}
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

// hydrateMemberHistory rebuilds a memberHistory from a persisted snapshot.
// Networks entries last updated more than maxAge before now are dropped, and
// the least recently updated beyond maxNetworksPerMember. A snapshot without
// Networks is hydrated as current's history.
func hydrateMemberHistory(t *adapter.TagHistory, current uint64, now time.Time, maxAge, window time.Duration) *memberHistory {
	m := newMemberHistory()
	if t == nil {
		return m
	}
	if len(t.Networks) == 0 {
		var successAt time.Time
		if t.LastSuccessDelayMs > 0 {
			successAt = t.LastOutcomeAt
		}
		m.byNetwork[current] = hydrateLocalHistory(&adapter.NetworkHistory{
			LastSuccessDelayMs:  t.LastSuccessDelayMs,
			LastSuccessAt:       successAt,
			LastOutcomeAt:       t.LastOutcomeAt,
			ConsecutiveFailures: t.ConsecutiveFailures,
			UserFailures:        t.UserFailures,
			UpdatedAt:           t.UpdatedAt,
		}, now, window)
		return m
	}
	for env, n := range t.Networks {
		if n == nil || (maxAge > 0 && !n.UpdatedAt.IsZero() && now.Sub(n.UpdatedAt) > maxAge) {
			continue
		}
		m.byNetwork[env] = hydrateLocalHistory(n, now, window)
	}
	m.prune(now, maxAge, current)
	return m
}

// hydrateLocalHistory rebuilds a localHistory from one network's persisted
// snapshot.
func hydrateLocalHistory(t *adapter.NetworkHistory, now time.Time, window time.Duration) *localHistory {
	h := newLocalHistory()
	h.lastSuccessDelayMs = t.LastSuccessDelayMs
	h.lastSuccessAt = t.LastSuccessAt
	h.lastOutcomeAt = t.LastOutcomeAt
	h.consecutiveFailures = t.ConsecutiveFailures
	h.updatedAt = t.UpdatedAt
	if h.updatedAt.IsZero() {
		h.updatedAt = now
	}
	if len(t.UserFailures) > 0 {
		copies := make([]adapter.UserFailure, len(t.UserFailures))
		copy(copies, t.UserFailures)
		slices.SortFunc(copies, func(a, b adapter.UserFailure) int {
			return a.At.Compare(b.At)
		})
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
