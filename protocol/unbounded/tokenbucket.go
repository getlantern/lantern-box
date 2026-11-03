package unbounded

import (
	"container/list"
	"sync"
	"time"
)

// tokenBucket is a byte-rate limiter that grants tokens to waiters in the order
// they arrive. A waiter that gives up, because its socket closed or its
// deadline passed, leaves the queue, and if it was already granted its tokens
// go back, so bytes that were never sent cost nobody anything.
// golang.org/x/time/rate can't promise that: cancelling a reservation made
// before others refunds only what the later ones haven't been promised.
type tokenBucket struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	tokens  float64
	last    time.Time
	waiters list.List // of *bucketWaiter, oldest first
	// changed is closed, and replaced, whenever tokens come back or a waiter
	// leaves, so the waiters left can recompute when they'll be granted.
	changed chan struct{}
}

type bucketWaiter struct {
	n       float64
	elem    *list.Element
	granted bool
	// ready is closed once the waiter is granted its tokens.
	ready chan struct{}
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: time.Now(), changed: make(chan struct{})}
}

// Burst is the most tokens the bucket holds, and so the largest request it can
// ever grant.
func (b *tokenBucket) Burst() int { return int(b.burst) }

// Limit is the rate the bucket refills at, in tokens per second.
func (b *tokenBucket) Limit() float64 { return b.rate }

// Tokens is how many tokens the bucket holds now.
func (b *tokenBucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	return b.tokens
}

func (b *tokenBucket) refillLocked(now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
		b.last = now
	}
}

// grantLocked grants waiters at the head of the queue while the bucket holds
// enough for them.
func (b *tokenBucket) grantLocked() {
	for e := b.waiters.Front(); e != nil; e = b.waiters.Front() {
		w := e.Value.(*bucketWaiter)
		if b.tokens < w.n {
			return
		}
		b.tokens -= w.n
		b.waiters.Remove(e)
		w.granted = true
		close(w.ready)
	}
}

// take asks for n tokens, n at most Burst. It returns nil if they were granted
// at once, and otherwise a waiter queued behind any earlier ones.
func (b *tokenBucket) take(n int) *bucketWaiter {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	if b.waiters.Len() == 0 && b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return nil
	}
	w := &bucketWaiter{n: float64(n), ready: make(chan struct{})}
	w.elem = b.waiters.PushBack(w)
	return w
}

// poll refills the bucket and grants what it can. It reports whether w has
// been granted, and if not, how long until the bucket could cover w and every
// waiter ahead of it, and a channel closed if that changes sooner.
func (b *tokenBucket) poll(w *bucketWaiter) (bool, time.Duration, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	b.grantLocked()
	if w.granted {
		return true, 0, nil
	}
	need := -b.tokens
	for e := b.waiters.Front(); e != nil; e = e.Next() {
		need += e.Value.(*bucketWaiter).n
		if e == w.elem {
			break
		}
	}
	return false, time.Duration(need / b.rate * float64(time.Second)), b.changed
}

// cancel withdraws w: out of the queue if it's still waiting, or its tokens
// back in the bucket if it was granted meanwhile. Either way the waiters
// behind it may now be granted.
func (b *tokenBucket) cancel(w *bucketWaiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	if w.granted {
		b.tokens = min(b.burst, b.tokens+w.n)
	} else {
		b.waiters.Remove(w.elem)
	}
	b.grantLocked()
	b.notifyLocked()
}

// refund puts back n tokens granted for I/O that then failed, so unsent bytes
// cost nothing.
func (b *tokenBucket) refund(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	b.tokens = min(b.burst, b.tokens+float64(n))
	b.grantLocked()
	b.notifyLocked()
}

func (b *tokenBucket) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}
