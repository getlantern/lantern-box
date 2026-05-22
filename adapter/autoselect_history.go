package adapter

import (
	"sync"
	"time"
)

// TagHistory is a point-in-time snapshot of one member's selection
// state, mirroring the in-memory history kept by the MutableAutoSelect
// group. Probe outcomes feed the scalar fields; real user-traffic
// outcomes (dial errors, data-plane stalls) feed the UserFailures
// sliding window. The two tracks are kept separate so a censor that
// lets the probe URL through while dropping user traffic shows up as
// elevated UserFailures with healthy probe scalars simultaneously.
type TagHistory struct {
	// LastSuccessDelayMs is the most recent successful probe RTT
	// (clamped to ≥1 ms). 0 means "no successful probe yet" — rank
	// uses it as a sentinel to deprioritize against measured peers.
	LastSuccessDelayMs uint32 `json:"last_success_delay_ms,omitempty"`
	// LastOutcomeAt is the timestamp of the most recent probe outcome
	// (success or failure). Used by the probe-cycle freshSince gate to
	// distinguish "probed this cycle" from "stale outcome."
	LastOutcomeAt time.Time `json:"last_outcome_at,omitempty"`
	// ConsecutiveFailures counts probe failures since the last probe
	// success. Resets to zero on probe success.
	ConsecutiveFailures uint32 `json:"consecutive_failures,omitempty"`
	// UserFailures is the sliding window of user-traffic failure
	// timestamps (dial errors and data-plane stalls). Probe successes
	// never enter this window; entries age out naturally on the next
	// mutation older than the group's userFailureWindow.
	UserFailures []time.Time `json:"user_failures,omitempty"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

// AutoSelectHistoryStorage is the store the MutableAutoSelect group
// writes to whenever a member's history changes.
//
// Implementations must be safe for concurrent use. Store and Delete
// must be non-blocking and fast: the group calls them while holding
// internal locks, so blocking work — disk I/O, network calls, slow
// serialization — must happen out-of-band via the hook, not inline.
// When a state change actually occurs, the hook fires after the change
// is visible to subsequent Load callers. A Store of nil is equivalent
// to Delete. Calls that arrive after Close are dropped without panic.
type AutoSelectHistoryStorage interface {
	Load(tag string) *TagHistory
	Store(tag string, h *TagHistory)
	Delete(tag string)
	All() map[string]*TagHistory
	SetHook(hook func(tag string))
	Close() error
}

// NewAutoSelectHistoryStorage returns an in-memory
// AutoSelectHistoryStorage. The returned storage is empty; hosts seed
// it by calling Store for each entry restored from disk before
// registering it in the service context.
func NewAutoSelectHistoryStorage() AutoSelectHistoryStorage {
	return &memoryAutoSelectHistoryStorage{entries: make(map[string]*TagHistory)}
}

type memoryAutoSelectHistoryStorage struct {
	mu      sync.RWMutex
	entries map[string]*TagHistory
	hook    func(tag string)
}

func (s *memoryAutoSelectHistoryStorage) Load(tag string) *TagHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneTagHistory(s.entries[tag])
}

func (s *memoryAutoSelectHistoryStorage) Store(tag string, h *TagHistory) {
	if h == nil {
		s.Delete(tag)
		return
	}
	s.mu.Lock()
	if s.entries == nil {
		// Closed. Probe / dataplane goroutines can outlive the
		// storage on tunnel shutdown (group.Close cancels its
		// context but does not block on in-flight callbacks); a
		// nil-map assignment here would panic the writer's
		// goroutine. Dropping the late write is the safe choice.
		s.mu.Unlock()
		return
	}
	s.entries[tag] = cloneTagHistory(h)
	hook := s.hook
	s.mu.Unlock()
	if hook != nil {
		hook(tag)
	}
}

func (s *memoryAutoSelectHistoryStorage) Delete(tag string) {
	s.mu.Lock()
	if s.entries == nil {
		s.mu.Unlock()
		return
	}
	_, existed := s.entries[tag]
	delete(s.entries, tag)
	hook := s.hook
	s.mu.Unlock()
	if existed && hook != nil {
		hook(tag)
	}
}

func (s *memoryAutoSelectHistoryStorage) All() map[string]*TagHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*TagHistory, len(s.entries))
	for tag, h := range s.entries {
		out[tag] = cloneTagHistory(h)
	}
	return out
}

func (s *memoryAutoSelectHistoryStorage) SetHook(hook func(tag string)) {
	s.mu.Lock()
	s.hook = hook
	s.mu.Unlock()
}

func (s *memoryAutoSelectHistoryStorage) Close() error {
	s.mu.Lock()
	s.hook = nil
	s.entries = nil
	s.mu.Unlock()
	return nil
}

// LatestSuccessDelay returns LastSuccessDelayMs, or 0 when h is nil.
// Kept as a method for symmetry with LatestSuccessTime and to let
// callers handle nil snapshots without an extra branch.
func (h *TagHistory) LatestSuccessDelay() uint32 {
	if h == nil {
		return 0
	}
	return h.LastSuccessDelayMs
}

// LatestSuccessTime is best-effort: the spec only persists the most
// recent outcome timestamp (success or failure), so callers asking
// "tested N seconds ago" get UpdatedAt as a usable reference even when
// the most recent outcome was a failure. Returns the zero time when h
// is nil.
func (h *TagHistory) LatestSuccessTime() time.Time {
	if h == nil {
		return time.Time{}
	}
	return h.UpdatedAt
}

func cloneTagHistory(h *TagHistory) *TagHistory {
	if h == nil {
		return nil
	}
	out := *h
	if len(h.UserFailures) > 0 {
		out.UserFailures = make([]time.Time, len(h.UserFailures))
		copy(out.UserFailures, h.UserFailures)
	}
	return &out
}
