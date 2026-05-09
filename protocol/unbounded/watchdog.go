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
// broflake; landing this passive observer first gives us empirical data on
// how often the symptom actually fires before we touch FSM ownership.
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
//   - "Any fast dial in window" is read from the rolling buffer, not a
//     per-pairing flag, so re-arm happens automatically as soon as a fresh
//     pairing produces a fast dial — no need to detect "new pairing" from
//     the outbound layer.
//   - Window size and consecutive-slow limit (10 and 5 by default) make the
//     "any fast dial saves the pairing" property concrete: even a single
//     sub-1s sample anywhere in the last 10 dials prevents a trip.
type dialWatchdog struct {
	mu sync.Mutex

	// Fixed-size circular buffer of recent dial elapsed times. We use a
	// true ring (write index + filled flag) instead of a slice slid via
	// append + reslice. The slice approach steadily eats the underlying
	// array's capacity (each `samples = samples[1:]` advances the slice's
	// start offset) and forces a reallocation roughly every `window`
	// records. Since record() runs on every dial — potentially hundreds
	// per minute on an active client — we want to avoid the churn.
	ring     []time.Duration
	writeIdx int
	filled   bool

	tripped    bool
	tripsTotal int

	// Configurable, but defaults are baked in. Exposed as fields so tests
	// can shrink the trip cap and thresholds without sleeping.
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

// newDialWatchdog constructs a watchdog using the package-level defaults.
// Tests construct via the struct literal directly to override individual
// fields; that path goes through validate() too via a constructor wrapper
// in the test file (newTestWatchdog).
func newDialWatchdog() *dialWatchdog {
	w := &dialWatchdog{
		fastThreshold:       watchdogFastThreshold,
		slowThreshold:       watchdogSlowThreshold,
		window:              watchdogWindow,
		consecutiveSlowMin:  watchdogConsecutiveSlowMin,
		maxTripsPerLifetime: watchdogMaxTripsPerLifetime,
	}
	w.init()
	return w
}

// init validates configuration and allocates the ring. Called once at
// construction. Misconfiguration is clamped rather than panicked: the
// watchdog is a debug aid, not load-bearing logic, and a zero-window or
// inverted limit must not bring down a Lantern client.
func (w *dialWatchdog) init() {
	if w.window <= 0 {
		w.window = 1
	}
	if w.consecutiveSlowMin <= 0 {
		w.consecutiveSlowMin = 1
	}
	if w.consecutiveSlowMin > w.window {
		w.consecutiveSlowMin = w.window
	}
	if w.maxTripsPerLifetime < 0 {
		w.maxTripsPerLifetime = 0
	}
	w.ring = make([]time.Duration, w.window)
	w.writeIdx = 0
	w.filled = false
}

// watchdogEvent is the verdict from a single record() call. The two booleans
// are state-transition signals — at most one is true on any given return,
// and most calls return both false (no transition; suppressed write while
// tripped, ramp-up before window is full, no-op while medium-band, etc.).
// The caller is expected to switch on `tripped` / `rearmed` to decide
// whether to log anything; the remaining fields are diagnostic context.
type watchdogEvent struct {
	tripped     bool          // a new trip just fired
	rearmed     bool          // a fast dial cleared a previously-tripped state
	reason      string        // human-readable trip cause, set on tripped
	medianSlow  time.Duration // median elapsed of the slow tail, set on tripped
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

	// Write into the ring and advance.
	w.ring[w.writeIdx] = elapsed
	w.writeIdx++
	if w.writeIdx >= len(w.ring) {
		w.writeIdx = 0
		w.filled = true
	}

	count := w.sampleCountLocked()

	// Re-arm: a fast dial while tripped clears the trip flag. We log the
	// transition once so a reader of lantern.log can see when a pairing
	// recovered (whether by broflake re-pairing under the hood, or by the
	// peer's network conditions improving).
	if w.tripped && elapsed < w.fastThreshold {
		w.tripped = false
		return watchdogEvent{
			rearmed:     true,
			tripsTotal:  w.tripsTotal,
			sampleCount: count,
		}
	}

	// While currently tripped, suppress further trip events: we already
	// emitted the warning, no signal in repeating it on every slow dial.
	if w.tripped {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: count}
	}

	// Need a full window before evaluating; otherwise a startup burst of
	// 5 slow dials would always trip on first pair.
	if !w.filled {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: count}
	}

	if w.tripsTotal >= w.maxTripsPerLifetime {
		return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: count}
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
	for _, s := range w.ring {
		if s < w.fastThreshold {
			return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: count}
		}
	}
	tail := w.recentLocked(w.consecutiveSlowMin)
	for _, s := range tail {
		if s <= w.slowThreshold {
			return watchdogEvent{tripsTotal: w.tripsTotal, sampleCount: count}
		}
	}

	w.tripped = true
	w.tripsTotal++
	return watchdogEvent{
		tripped:     true,
		reason:      "no fast dial in window and >=5 consecutive slow",
		medianSlow:  medianDuration(tail),
		tripsTotal:  w.tripsTotal,
		sampleCount: count,
	}
}

// sampleCountLocked returns the number of valid entries in the ring.
// Caller must hold w.mu.
func (w *dialWatchdog) sampleCountLocked() int {
	if w.filled {
		return len(w.ring)
	}
	return w.writeIdx
}

// recentLocked returns the n most-recent samples in arrival order (oldest
// of the n first, newest last). Caller must hold w.mu and ensure
// n <= sampleCountLocked().
func (w *dialWatchdog) recentLocked(n int) []time.Duration {
	out := make([]time.Duration, n)
	// Walk backwards from the most recently written slot.
	for i := 0; i < n; i++ {
		idx := (w.writeIdx - 1 - i + len(w.ring)) % len(w.ring)
		out[n-1-i] = w.ring[idx]
	}
	return out
}

// snapshot returns a copy of the current ring contents in arrival order
// (oldest first), for diagnostic logging. Caller is free to log/format
// without holding the watchdog mutex.
func (w *dialWatchdog) snapshot() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := w.sampleCountLocked()
	if count == 0 {
		return nil
	}
	return w.recentLocked(count)
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
