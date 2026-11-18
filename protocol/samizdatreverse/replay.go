package samizdatreverse

import (
	"sync"
	"time"
)

const (
	// replayWindow and replayCapacity bound how long and how many admitted
	// SessionIDs the portal remembers: each for between one and two windows,
	// or less once a window holds replayCapacity. A portal admits a few
	// sessions per pairing, so the cap only matters under abuse.
	replayWindow   = 24 * time.Hour
	replayCapacity = 100_000
)

// seenSessionIDs remembers SessionIDs in two generations: new ones go into
// the current one, both are searched, and rotation drops the older. It
// mirrors samizdat's own replay filter, which the portal's identity servers
// leave off because they can't masquerade.
type seenSessionIDs struct {
	mu        sync.Mutex
	window    time.Duration
	capacity  int
	cur, prev map[string]struct{}
	rotatedAt time.Time
}

func newSeenSessionIDs(window time.Duration, capacity int) *seenSessionIDs {
	return &seenSessionIDs{
		window:   window,
		capacity: capacity,
		cur:      make(map[string]struct{}),
	}
}

// seen reports whether sid was seen before, and remembers it if not.
func (s *seenSessionIDs) seen(sid []byte, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotatedAt.IsZero() {
		s.rotatedAt = now
	}
	if elapsed := now.Sub(s.rotatedAt); elapsed >= s.window {
		s.prev, s.cur = s.cur, make(map[string]struct{})
		if elapsed >= 2*s.window {
			s.prev = nil
		}
		s.rotatedAt = now.Add(-(elapsed % s.window))
	}
	k := string(sid)
	if _, ok := s.cur[k]; ok {
		return true
	}
	if _, ok := s.prev[k]; ok {
		return true
	}
	if len(s.cur) >= s.capacity {
		s.prev, s.cur = s.cur, make(map[string]struct{})
		s.rotatedAt = now
	}
	s.cur[k] = struct{}{}
	return false
}
