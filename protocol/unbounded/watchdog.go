package unbounded

import (
	"sort"
	"sync"
	"time"
)

// dialWatchdog tracks the elapsed time of recent successful dials through the
// unbounded outbound and flags pairings that look unhealthy. It is purely
// observational: on a trip it returns information for the caller to log, but
// takes no action against the broflake state machine. The reset-on-trip
// behavior is deferred to a follow-up that adds a soft re-pairing API in
// broflake (see PR #357 et al.); landing this passive observer first gives
// us empirical data on how often the symptom actually fires before we touch
// FSM ownership.
//
// Design rationale (region-agnostic by construction):
//
//   - The thresholds (watchdogFastThreshold, watchdogSlowThreshold) are not
//     "what is good in the US"; they are "what's clearly fast anywhere a
//     working WebRTC datachannel would be" and "what's clearly degenerate
//     anywhere". A pairing that hits 5+ consecutive dials in the slow band
//     with no fast dials anywhere in the window is broken regardless of the
//     consumer's geography. Networks in restrictive regions tend to land
//     consistently in the 1–4s band — slower than ideal but not >5s, so
//     they don't trip.
//   - everSeenFast is read from the rolling window, not a per-pairing flag,
//     so re-arm happens automatically as soon as a fresh pairing produces
//     a fast dial — no need to detect "new pairing" from the outbound layer.
//   - The window size matches the consecutive-slow limit: with N=10 and
//     limit=5, we trip when 5 of the last 10 dials are slow AND none are
//     fast. That's the "any fast dial saves the pairing" property the
//     design called for.
type dialWatchdog struct {
	mu         sync.Mutex
	samples    []time.Duration // ring buffer, capped at window
	tripped    bool
	tripsTotal int

	// Configurable, but defaults are baked in. Exposed as fields so tests
	// can shrink the window / thresholds without sleeping.
	fastThreshold       time.Duration
	slowThreshold       time.Duration
	window              int
	consecutiveSlowMin  int
	maxTripsPerLifetime int
}

const (
	watchdogFastThreshold       = 1 * time.Second
	watchdogSlowThreshold       = 5 * time.Second
	watchdogWindow              = 10
	watchdogConsecutiveSlowMin  = 5
	watchdogMaxTripsPerLifetime = 1000 // safety cap on log-spam
)

func newDialWatchdog() *dialWatchdog {
	return &dialWatchdog{
		fastThreshold:       watchdogFastThreshold,
		slowThreshold:       watchdogSlowThreshold,
		window:              watchdogWindow,
		consecutiveSlowMin:  watchdogConsecutiveSlowMin,
		maxTripsPerLifetime: watchdogMaxTripsPerLifetime,
	}
}

// watchdogEvent is the verdict from a single record() call. Exactly one of
// the boolean fields is true on any return: the caller can switch on it to
// decide whether to log anything (and at what level).
type watchdogEvent struct {
	tripped     bool          // a new trip just fired
	rearmed     bool          // a fast dial cleared a previously-tripped state
	reason      string        // human-readable trip cause, set on tripped
	medianSlow  time.Duration // median elapsed of the slow window, set on tripped
	tripsTotal  int           // cumulative trips this watchdog has fired
	sampleCount int           // current ring-buffer fill
}

// record ingests a successful dial's elapsed time and returns whether the
// pairing's health verdict changed. Failed dials should NOT be passed in:
// they conflate peer quality with destination availability.
//
// Concurrency-safe; record is called from every concurrent dial.
func (w *dialWatchdog) record(elapsed time.Duration) watchdogEvent {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.samples = append(w.samples, elapsed)
	if len(w.samples) > w.window {
		w.samples = w.samples[1:]
	}

	// Re-arm: a fast dial while tripped clears the trip flag. We log the
	// transition once so a reader of lantern.log can see when a pairing
	// recovered (whether by broflake re-pairing under the hood, or by the
	// peer's network conditions improving).
	if w.tripped && elapsed < w.fastThreshold {
		w.tripped = false
		return watchdogEvent{
			rearmed:     true,
			tripsTotal:  w.tripsTotal,
			sampleCount: len(w.samples),
		}
	}

	// While currently tripped, suppress further trip events: we already
	// emitted the warning, no signal in repeating it on every slow dial.
	if w.tripped {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: len(w.samples)}
	}

	// Need a full window before evaluating; otherwise a startup burst of
	// 5 slow dials would always trip on first pair.
	if len(w.samples) < w.window {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: len(w.samples)}
	}

	if w.tripsTotal >= w.maxTripsPerLifetime {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: len(w.samples)}
	}

	// Trip if the most recent consecutiveSlowMin dials are all slow AND
	// no dial in the whole window is fast. Both conditions are necessary:
	//
	//   - The "no fast in window" guard saves pairings that have proved
	//     they CAN do fast dials but happen to have a slow burst at the
	//     moment we evaluate.
	//   - The "consecutive slow" guard ensures we don't trip on an
	//     intermittently slow pairing that's interleaving fast dials with
	//     occasional slow ones at a tolerable rate.
	for _, s := range w.samples {
		if s < w.fastThreshold {
			return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: len(w.samples)}
		}
	}
	tail := w.samples[len(w.samples)-w.consecutiveSlowMin:]
	for _, s := range tail {
		if s <= w.slowThreshold {
			return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: len(w.samples)}
		}
	}

	w.tripped = true
	w.tripsTotal++
	return watchdogEvent{
		tripped:     true,
		reason:      "no fast dial in window and >=5 consecutive slow",
		medianSlow:  medianDuration(tail),
		tripsTotal:  w.tripsTotal,
		sampleCount: len(w.samples),
	}
}

// snapshot returns a copy of the current ring buffer for diagnostic logging.
// Caller is free to log/format without holding the watchdog mutex.
func (w *dialWatchdog) snapshot() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]time.Duration, len(w.samples))
	copy(out, w.samples)
	return out
}

func medianDuration(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}
