package unbounded

import (
	"testing"
	"time"
)

// newTestWatchdog builds a watchdog with the production thresholds and
// window so the tests exercise the same code paths real Lantern clients
// will. (An earlier draft set "shorter windows" here, but every test
// reasoned in terms of the production window/limit pair, so shrinking
// only made the tests harder to read without exercising new branches.)
// Tests that need to bypass the trip cap can set maxTripsPerLifetime
// after construction.
func newTestWatchdog() *dialWatchdog {
	w := &dialWatchdog{
		fastThreshold:       1 * time.Second,
		slowThreshold:       5 * time.Second,
		window:              10,
		consecutiveSlowMin:  5,
		maxTripsPerLifetime: 1000,
	}
	w.init()
	return w
}

// feed N samples of the same elapsed value and return the events emitted.
func feed(t *testing.T, w *dialWatchdog, elapsed time.Duration, n int) []watchdogEvent {
	t.Helper()
	out := make([]watchdogEvent, n)
	for i := 0; i < n; i++ {
		out[i] = w.record(elapsed)
	}
	return out
}

// The classic "stuck on bad peer" symptom: 10 consecutive slow dials, no fast
// dial anywhere in the window. The 10th sample should trip exactly once.
func TestWatchdog_TripsAfterFullWindowOfSlowDialsWithNoFast(t *testing.T) {
	w := newTestWatchdog()

	events := feed(t, w, 12*time.Second, 10)

	// First 9 samples build up the window without tripping (we don't
	// evaluate until the window is full).
	for i := 0; i < 9; i++ {
		if events[i].tripped {
			t.Fatalf("trip at sample %d (window not full yet); got %+v", i, events[i])
		}
	}
	if !events[9].tripped {
		t.Fatalf("expected trip on 10th sample; got %+v", events[9])
	}
	if events[9].tripsTotal != 1 {
		t.Errorf("tripsTotal: want 1, got %d", events[9].tripsTotal)
	}
	if events[9].medianSlow == 0 {
		t.Errorf("expected non-zero medianSlow on trip")
	}

	// Subsequent slow dials must NOT re-trip — the "while tripped, suppress"
	// branch keeps log spam down.
	more := feed(t, w, 12*time.Second, 5)
	for i, e := range more {
		if e.tripped {
			t.Errorf("re-trip on suppressed slow dial #%d: %+v", i, e)
		}
	}
}

// Even one fast dial anywhere in the window saves the pairing — the
// region-agnostic property of the design. Networks that produce a 4s + a
// 500ms + a 12s mix should NOT trip; the fast dial is evidence the peer can
// do better.
func TestWatchdog_DoesNotTripIfAnyFastDialInWindow(t *testing.T) {
	w := newTestWatchdog()

	// 9 slow + 1 fast (interleaved as last) → fast guard stops the trip.
	for i := 0; i < 9; i++ {
		_ = w.record(12 * time.Second)
	}
	ev := w.record(500 * time.Millisecond)
	if ev.tripped {
		t.Fatalf("tripped despite a fast dial in window: %+v", ev)
	}
}

// In a region with structurally slow paths (everything 2-4s), no dial is
// "fast" but no consecutive run is "slow" either → no trip. This is the
// case where region-baseline thresholds would have been wrong.
func TestWatchdog_DoesNotTripOnConsistentlyMediumDials(t *testing.T) {
	w := newTestWatchdog()
	for i := 0; i < 50; i++ {
		ev := w.record(3 * time.Second)
		if ev.tripped {
			t.Fatalf("tripped on medium-band dial #%d: %+v", i, ev)
		}
	}
}

// Trip → re-arm → trip again. The re-arm path needs to actually clear the
// tripped flag, otherwise the second trip never fires.
func TestWatchdog_RearmsOnFastDialAfterTrip(t *testing.T) {
	w := newTestWatchdog()

	// Trip first.
	feed(t, w, 12*time.Second, 10)
	if !w.tripped {
		t.Fatal("setup: expected watchdog to be tripped")
	}

	// Fast dial → re-arm.
	ev := w.record(300 * time.Millisecond)
	if !ev.rearmed {
		t.Fatalf("expected rearmed event; got %+v", ev)
	}
	if w.tripped {
		t.Fatalf("watchdog still tripped after rearm: %+v", w)
	}

	// And the watchdog must be able to trip a second time after rearming.
	// The rearming window is now 1 fast + 9 slow's worth of buffer; we need
	// to push 10 more slow dials to evict the fast one.
	feed(t, w, 12*time.Second, 9)  // window now: 1 fast + 9 slow → fast guard still active
	if w.tripped {
		t.Fatalf("tripped while fast dial still in window: %+v", w)
	}
	ev2 := w.record(12 * time.Second) // evicts the fast → window is all slow
	if !ev2.tripped {
		t.Fatalf("expected second trip after fast evicted; window=%v", w.snapshot())
	}
	if ev2.tripsTotal != 2 {
		t.Errorf("tripsTotal: want 2, got %d", ev2.tripsTotal)
	}
}

// Safety cap: don't keep emitting trip events forever even if the peer is
// degenerately bad and we can't auto-reset (that's phase-2 work). This caps
// log spam so a stuck client doesn't write GB of warnings overnight.
func TestWatchdog_MaxTripsCapsTrips(t *testing.T) {
	w := newTestWatchdog()
	w.maxTripsPerLifetime = 2 // safe to mutate post-init: ring already allocated

	// First trip.
	feed(t, w, 12*time.Second, 10)
	// Re-arm + trip.
	w.record(300 * time.Millisecond)
	feed(t, w, 12*time.Second, 10)
	// Re-arm + would-be-trip-3 (should be suppressed by cap).
	w.record(300 * time.Millisecond)
	events := feed(t, w, 12*time.Second, 10)

	for i, ev := range events {
		if ev.tripped {
			t.Fatalf("third trip fired despite maxTripsPerLifetime=2 at index %d: %+v", i, ev)
		}
	}
	if w.tripsTotal != 2 {
		t.Errorf("tripsTotal: want 2 (cap), got %d", w.tripsTotal)
	}
}

// init() clamps invalid configurations rather than panicking — the watchdog
// is a debug aid, and a misconfigured one must not bring down the Lantern
// client. Cover a few representative degenerate inputs and assert the
// invariants we rely on (window > 0, consecutiveSlowMin in [1, window]).
func TestWatchdog_InitClampsBadConfig(t *testing.T) {
	cases := []struct {
		name                 string
		setup                func(*dialWatchdog)
		wantWindow           int
		wantConsecutiveMin   int
		wantMaxTripsAtLeast0 bool
	}{
		{
			name:               "zero window",
			setup:              func(w *dialWatchdog) { w.window = 0; w.consecutiveSlowMin = 5 },
			wantWindow:         1,
			wantConsecutiveMin: 1, // clamped down to fit window
		},
		{
			name:               "negative window",
			setup:              func(w *dialWatchdog) { w.window = -3; w.consecutiveSlowMin = 5 },
			wantWindow:         1,
			wantConsecutiveMin: 1,
		},
		{
			name:               "consecutiveSlowMin > window",
			setup:              func(w *dialWatchdog) { w.window = 4; w.consecutiveSlowMin = 99 },
			wantWindow:         4,
			wantConsecutiveMin: 4, // clamped to window so the recent-tail slice doesn't go OOB
		},
		{
			name:                 "negative maxTrips",
			setup:                func(w *dialWatchdog) { w.window = 10; w.consecutiveSlowMin = 5; w.maxTripsPerLifetime = -1 },
			wantWindow:           10,
			wantConsecutiveMin:   5,
			wantMaxTripsAtLeast0: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &dialWatchdog{fastThreshold: 1 * time.Second, slowThreshold: 5 * time.Second}
			tc.setup(w)
			w.init()
			if w.window != tc.wantWindow {
				t.Errorf("window: want %d, got %d", tc.wantWindow, w.window)
			}
			if w.consecutiveSlowMin != tc.wantConsecutiveMin {
				t.Errorf("consecutiveSlowMin: want %d, got %d", tc.wantConsecutiveMin, w.consecutiveSlowMin)
			}
			if tc.wantMaxTripsAtLeast0 && w.maxTripsPerLifetime < 0 {
				t.Errorf("maxTripsPerLifetime: want >=0, got %d", w.maxTripsPerLifetime)
			}
			// Most important: a record() against a clamped watchdog must
			// not panic. Feed a few samples; we don't care about the
			// trip verdict here, only that the ring indexing is safe.
			for i := 0; i < 20; i++ {
				_ = w.record(2 * time.Second)
			}
		})
	}
}
