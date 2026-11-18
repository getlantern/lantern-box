package unbounded

import (
	"testing"
	"time"
)

// Waits withdrawn in the order they queued leave no debt: a later request
// waits only for its own tokens. With x/time/rate, the first withdrawal would
// refund nothing because the second was queued after it.
func TestTokenBucketWithdrawnWaitsLeaveNoDebt(t *testing.T) {
	const rate, n = 1024.0, 8 * 1024
	b := newTokenBucket(rate, n)
	if w := b.take(n); w != nil {
		t.Fatal("the full burst wasn't granted at once")
	}
	first, second := b.take(n), b.take(n)
	if first == nil || second == nil {
		t.Fatal("requests on an empty bucket were granted")
	}
	b.cancel(first)
	b.cancel(second)

	later := b.take(n)
	if later == nil {
		t.Fatal("the bucket refilled implausibly fast")
	}
	defer b.cancel(later)
	if _, wait, _ := b.poll(later); wait > time.Duration(1.1*n/rate*float64(time.Second)) {
		t.Fatalf("a later request waits %v, as if withdrawn waits were still charged; want about %v",
			wait, time.Duration(n/rate*float64(time.Second)))
	}
}

// Tokens go to waiters in the order they arrived: a small request doesn't
// jump a large one queued ahead of it.
func TestTokenBucketGrantsInOrder(t *testing.T) {
	b := newTokenBucket(1024, 8*1024)
	b.take(8 * 1024)
	big := b.take(4 * 1024)
	small := b.take(1)
	if big == nil || small == nil {
		t.Fatal("requests on an empty bucket were granted")
	}
	b.mu.Lock()
	b.tokens = 1024 // enough for small, not big
	b.last = time.Now()
	b.mu.Unlock()
	if granted, _, _ := b.poll(small); granted {
		t.Fatal("a small request was granted ahead of a larger one queued before it")
	}
	b.mu.Lock()
	b.tokens = 4*1024 + 1
	b.mu.Unlock()
	if granted, _, _ := b.poll(small); !granted {
		t.Fatal("once the head was covered, the waiters behind it weren't granted")
	}
	select {
	case <-big.ready:
	default:
		t.Fatal("the head waiter wasn't granted")
	}
}

// A waiter withdrawn after it was granted puts its tokens back.
func TestTokenBucketWithdrawnGrantIsRefunded(t *testing.T) {
	b := newTokenBucket(1024, 8*1024)
	b.take(8 * 1024)
	w := b.take(1024)
	b.mu.Lock()
	b.tokens = 1024
	b.last = time.Now()
	b.grantLocked()
	b.mu.Unlock()
	if !w.granted {
		t.Fatal("the waiter wasn't granted")
	}
	before := b.Tokens()
	b.cancel(w)
	if got := b.Tokens() - before; got < 1023 {
		t.Fatalf("withdrawing a granted waiter refunded %.0f tokens, want 1024", got)
	}
}

// When a waiter ahead withdraws, those behind it recompute when they'll be
// granted, rather than sleeping out the time the withdrawn demand would have
// taken.
func TestTokenBucketWithdrawalWakesTheWaitersBehind(t *testing.T) {
	b := newTokenBucket(1024, 8*1024)
	b.take(8 * 1024)
	ahead := newConnLimits(b, b)
	behind := newConnLimits(b, b)
	aheadDone := make(chan error, 1)
	go func() { aheadDone <- ahead.wait(b, 8*1024, true) }() // ~8s of tokens
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	behindDone := make(chan error, 1)
	go func() { behindDone <- behind.wait(b, 1024, true) }() // ~1s more, behind it
	time.Sleep(100 * time.Millisecond)
	ahead.close()
	if err := <-aheadDone; err == nil {
		t.Fatal("the withdrawn wait succeeded")
	}
	select {
	case err := <-behindDone:
		if err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited > 3*time.Second {
			t.Fatalf("the waiter behind took %v, as if the withdrawn demand still stood", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter behind slept out the withdrawn demand")
	}
}
