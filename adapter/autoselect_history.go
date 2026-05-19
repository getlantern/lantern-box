package adapter

import (
	"sync"
	"time"
)

// Outcome classifies the result of a probe or user dial attempt. The
// enum value is what gets persisted, so additions must append rather
// than reorder.
type Outcome uint8

const (
	OutcomeSuccess Outcome = iota
	OutcomeTimeout
	OutcomeHandshakeFailure
)

// TimestampedOutcome is one entry in a member's recent-outcome ring.
type TimestampedOutcome struct {
	Outcome   Outcome   `json:"outcome"`
	DelayMs   uint32    `json:"delay_ms,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// TagHistory is a point-in-time snapshot of one member's selection
// state, mirroring the in-memory history kept by the MutableAutoSelect
// group.
type TagHistory struct {
	Outcomes []TimestampedOutcome `json:"outcomes,omitempty"`
	// ConsecutiveFailures resets to zero on the next probe success.
	ConsecutiveFailures uint32 `json:"consecutive_failures,omitempty"`
	// UserFailures counts real user-traffic failures (dial errors +
	// data-plane stalls). Persisting it means a server that was about
	// to be demoted on user traffic doesn't come back innocent after
	// a tunnel close/open inside the current poll window. Laundering-
	// resistant: probe successes do not decrement it; only a
	// successful read on a wrapped data-plane conn resets it.
	UserFailures uint32    `json:"user_failures,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
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

// LatestSuccessDelay returns the DelayMs of the most recent
// OutcomeSuccess in h.Outcomes, or 0 when no success is recorded.
func (h *TagHistory) LatestSuccessDelay() uint32 {
	if h == nil {
		return 0
	}
	for i := len(h.Outcomes) - 1; i >= 0; i-- {
		if h.Outcomes[i].Outcome == OutcomeSuccess {
			return h.Outcomes[i].DelayMs
		}
	}
	return 0
}

// LatestSuccessTime returns the Timestamp of the most recent
// OutcomeSuccess in h.Outcomes, or UpdatedAt when no success exists.
// Falls back to UpdatedAt rather than the zero time so callers that
// render "tested N seconds ago" still have a usable reference point
// after a string of failures.
func (h *TagHistory) LatestSuccessTime() time.Time {
	if h == nil {
		return time.Time{}
	}
	for i := len(h.Outcomes) - 1; i >= 0; i-- {
		if h.Outcomes[i].Outcome == OutcomeSuccess {
			return h.Outcomes[i].Timestamp
		}
	}
	return h.UpdatedAt
}

func cloneTagHistory(h *TagHistory) *TagHistory {
	if h == nil {
		return nil
	}
	out := *h
	if len(h.Outcomes) > 0 {
		out.Outcomes = make([]TimestampedOutcome, len(h.Outcomes))
		copy(out.Outcomes, h.Outcomes)
	}
	return &out
}
