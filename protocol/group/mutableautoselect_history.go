package group

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/lantern-box/adapter"
)

// localOutcome mirrors adapter.Outcome with the same numeric values so
// hydrate/snapshot convert by direct cast. Additions must append in both
// enums to keep the cast valid.
type localOutcome uint8

const (
	outcomeSuccess localOutcome = iota
	outcomeTimeout
	outcomeHandshakeFailure
)

func (o localOutcome) isSuccess() bool { return o == outcomeSuccess }

type timestampedOutcome struct {
	outcome   localOutcome
	delayMs   uint32
	timestamp time.Time
}

const (
	defaultHistoryRingMax       = 20
	defaultSuccessRateWindow    = time.Hour
	defaultDemoteMinOutcomes    = 5
	defaultDemoteSuccessRate    = 0.3
	defaultConsecutiveFailLimit = 3
	defaultDataPlaneIdle        = 30 * time.Second
)

type historyParams struct {
	ringMax              int
	successRateWindow    time.Duration
	demoteSuccessRate    float64
	demoteMinOutcomes    uint32
	consecutiveFailLimit uint32
}

func defaultHistoryParams() historyParams {
	return historyParams{
		ringMax:              defaultHistoryRingMax,
		successRateWindow:    defaultSuccessRateWindow,
		demoteSuccessRate:    defaultDemoteSuccessRate,
		demoteMinOutcomes:    defaultDemoteMinOutcomes,
		consecutiveFailLimit: defaultConsecutiveFailLimit,
	}
}

// localHistory is the in-memory probe history for one server tag. The
// outcomes ring and consecutiveFailures track probe outcomes only.
// userFailures is the laundering-resistant user-traffic counter: a passing
// probe never decrements it; only a successful non-empty Read through a
// wrapped data-plane conn does.
type localHistory struct {
	mu                  sync.Mutex
	ringMax             int
	outcomes            []timestampedOutcome
	consecutiveFailures uint32
	userFailures        atomic.Uint32
}

func newLocalHistory(ringMax int) *localHistory {
	if ringMax <= 0 {
		ringMax = defaultHistoryRingMax
	}
	return &localHistory{ringMax: ringMax}
}

// now is a parameter so tests can drive a fake clock.
func (h *localHistory) append(o localOutcome, delayMs uint32, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := timestampedOutcome{outcome: o, delayMs: delayMs, timestamp: now}
	if o != outcomeSuccess {
		entry.delayMs = 0
	}
	if len(h.outcomes) < h.ringMax {
		h.outcomes = append(h.outcomes, entry)
	} else {
		copy(h.outcomes, h.outcomes[1:])
		h.outcomes[len(h.outcomes)-1] = entry
	}
	if o.isSuccess() {
		h.consecutiveFailures = 0
	} else {
		h.consecutiveFailures++
	}
}

func (h *localHistory) bumpUserFailures() { h.userFailures.Add(1) }

// resetUserFailures zeroes the user-traffic-failure counter and reports
// whether it was previously non-zero, so the hot path can skip a storage
// write when nothing changed.
func (h *localHistory) resetUserFailures() bool {
	return h.userFailures.Swap(0) != 0
}

func (h *localHistory) userFailureCount() uint32 { return h.userFailures.Load() }

func (h *localHistory) snapshot() ([]timestampedOutcome, uint32, uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]timestampedOutcome, len(h.outcomes))
	copy(out, h.outcomes)
	return out, h.consecutiveFailures, h.userFailures.Load()
}

// toTagHistory snapshots the localHistory into adapter.TagHistory form.
// updatedAt is a parameter so a single recordOutcome call uses one
// timestamp across the in-memory entry and the persisted snapshot.
func (h *localHistory) toTagHistory(updatedAt time.Time) *adapter.TagHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := &adapter.TagHistory{
		ConsecutiveFailures: h.consecutiveFailures,
		UserFailures:        h.userFailures.Load(),
		UpdatedAt:           updatedAt,
	}
	if len(h.outcomes) > 0 {
		out.Outcomes = make([]adapter.TimestampedOutcome, len(h.outcomes))
		for i, e := range h.outcomes {
			out.Outcomes[i] = adapter.TimestampedOutcome{
				Outcome:   adapter.Outcome(e.outcome),
				DelayMs:   e.delayMs,
				Timestamp: e.timestamp,
			}
		}
	}
	return out
}

// localizeOutcomes converts persisted entries to the in-memory form so
// lastSuccessDelay/demoted work uniformly on either source.
func localizeOutcomes(entries []adapter.TimestampedOutcome) []timestampedOutcome {
	if len(entries) == 0 {
		return nil
	}
	out := make([]timestampedOutcome, len(entries))
	for i, e := range entries {
		out[i] = timestampedOutcome{
			outcome:   localOutcome(e.Outcome),
			delayMs:   e.DelayMs,
			timestamp: e.Timestamp,
		}
	}
	return out
}

// hydrateLocalHistory rebuilds a localHistory from a persisted snapshot.
// Entries exceeding ringMax are dropped from the head so the in-memory
// ring stays bounded even if the persisted form was larger.
func hydrateLocalHistory(t *adapter.TagHistory, ringMax int) *localHistory {
	h := newLocalHistory(ringMax)
	if t == nil {
		return h
	}
	h.consecutiveFailures = t.ConsecutiveFailures
	h.userFailures.Store(t.UserFailures)
	if len(t.Outcomes) == 0 {
		return h
	}
	start := 0
	if len(t.Outcomes) > h.ringMax {
		start = len(t.Outcomes) - h.ringMax
	}
	h.outcomes = make([]timestampedOutcome, 0, len(t.Outcomes)-start)
	for _, e := range t.Outcomes[start:] {
		h.outcomes = append(h.outcomes, timestampedOutcome{
			outcome:   localOutcome(e.Outcome),
			delayMs:   e.DelayMs,
			timestamp: e.Timestamp,
		})
	}
	return h
}

func lastSuccessDelay(entries []timestampedOutcome) uint32 {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].outcome == outcomeSuccess {
			return entries[i].delayMs
		}
	}
	return 0
}

func hasOutcomeSince(entries []timestampedOutcome, t time.Time) bool {
	for i := len(entries) - 1; i >= 0; i-- {
		if !entries[i].timestamp.Before(t) {
			return true
		}
	}
	return false
}

// recentSuccessRate is successes/total over entries with age in window.
// With total == 0 the rate is meaningless; callers must check total
// before acting on rate. Ages are clamped to zero to tolerate clock skew.
func recentSuccessRate(entries []timestampedOutcome, now time.Time, window time.Duration) (rate float64, total uint32) {
	var succ uint32
	for _, e := range entries {
		age := max(0, now.Sub(e.timestamp))
		if age > window {
			continue
		}
		total++
		if e.outcome.isSuccess() {
			succ++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return float64(succ) / float64(total), total
}

// demoted is true when probe or user-traffic failures cross the
// consecutive-failure limit, or when the recent probe success rate dips
// below the threshold with enough sample size to trust the signal.
func demoted(entries []timestampedOutcome, consecFailures, userFailures uint32, now time.Time, p historyParams) bool {
	if p.consecutiveFailLimit > 0 && consecFailures >= p.consecutiveFailLimit {
		return true
	}
	if p.consecutiveFailLimit > 0 && userFailures >= p.consecutiveFailLimit {
		return true
	}
	rate, total := recentSuccessRate(entries, now, p.successRateWindow)
	if total >= p.demoteMinOutcomes && rate < p.demoteSuccessRate {
		return true
	}
	return false
}
