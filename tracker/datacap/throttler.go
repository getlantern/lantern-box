package datacap

import (
	"context"
	"sync"
	"time"
)

// Throttler implements bandwidth throttling using a token bucket algorithm.
// This matches the approach used in http-proxy-lantern for consistent behavior.
type Throttler struct {
	mu sync.RWMutex

	enabled   bool
	readRate  int64 // Bytes per second for reads
	writeRate int64 // Bytes per second for writes

	// Token bucket for reads
	readTokens     float64
	readCapacity   float64
	readLastRefill time.Time

	// Token bucket for writes
	writeTokens     float64
	writeCapacity   float64
	writeLastRefill time.Time
}

// NewThrottler creates a new throttler with the specified bytes per second limit.
// Both read and write use the same rate initially.
func NewThrottler(bytesPerSec int64) *Throttler {
	return NewThrottlerWithRates(bytesPerSec, bytesPerSec)
}

// NewThrottlerWithRates creates a throttler with separate read and write rates.
// This allows asymmetric throttling (e.g., throttle downloads but not uploads).
func NewThrottlerWithRates(readBytesPerSec, writeBytesPerSec int64) *Throttler {
	now := time.Now()
	t := &Throttler{
		enabled:         readBytesPerSec > 0 || writeBytesPerSec > 0,
		readRate:        readBytesPerSec,
		writeRate:       writeBytesPerSec,
		readTokens:      float64(readBytesPerSec), // Start with full bucket
		readCapacity:    float64(readBytesPerSec), // 1 second worth of bytes
		readLastRefill:  now,
		writeTokens:     float64(writeBytesPerSec),
		writeCapacity:   float64(writeBytesPerSec),
		writeLastRefill: now,
	}
	return t
}

// Enable enables throttling with the specified rate for both read and write.
func (t *Throttler) Enable(bytesPerSec int64) {
	t.EnableWithRates(bytesPerSec, bytesPerSec)
}

// EnableWithRates enables throttling with separate read and write rates.
func (t *Throttler) EnableWithRates(readBytesPerSec, writeBytesPerSec int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.enabled = true
	t.readRate = readBytesPerSec
	t.writeRate = writeBytesPerSec
	t.readTokens = float64(readBytesPerSec)
	t.readCapacity = float64(readBytesPerSec)
	t.readLastRefill = now
	t.writeTokens = float64(writeBytesPerSec)
	t.writeCapacity = float64(writeBytesPerSec)
	t.writeLastRefill = now
}

// Disable disables throttling.
func (t *Throttler) Disable() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = false
}

// IsEnabled returns whether throttling is enabled.
func (t *Throttler) IsEnabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.enabled
}

// WaitRead waits until n bytes can be read according to the rate limit.
// This uses the token bucket algorithm: tokens are added continuously at the
// configured rate, and operations consume tokens. If not enough tokens are
// available, the operation blocks until sufficient tokens accumulate.
func (t *Throttler) WaitRead(ctx context.Context, n int) error {
	return t.wait(ctx, n, true)
}

// WaitWrite waits until n bytes can be written according to the rate limit.
func (t *Throttler) WaitWrite(ctx context.Context, n int) error {
	return t.wait(ctx, n, false)
}

// wait implements the token bucket algorithm for rate limiting.
func (t *Throttler) wait(ctx context.Context, n int, isRead bool) error {
	if n <= 0 {
		return nil
	}

	t.mu.Lock()

	if !t.enabled {
		t.mu.Unlock()
		return nil
	}

	// Select which bucket to use
	var tokens *float64
	var capacity *float64
	var lastRefill *time.Time
	var rate int64

	if isRead {
		tokens = &t.readTokens
		capacity = &t.readCapacity
		lastRefill = &t.readLastRefill
		rate = t.readRate
	} else {
		tokens = &t.writeTokens
		capacity = &t.writeCapacity
		lastRefill = &t.writeLastRefill
		rate = t.writeRate
	}

	// If rate is 0 or negative, no throttling
	if rate <= 0 {
		t.mu.Unlock()
		return nil
	}

	// Refill tokens based on time elapsed
	now := time.Now()
	elapsed := now.Sub(*lastRefill)
	tokensToAdd := elapsed.Seconds() * float64(rate)
	*tokens += tokensToAdd
	if *tokens > *capacity {
		*tokens = *capacity
	}
	*lastRefill = now

	// Check if we have enough tokens
	required := float64(n)
	if *tokens >= required {
		// Consume tokens and proceed immediately
		*tokens -= required
		t.mu.Unlock()
		return nil
	}

	// Not enough tokens - calculate wait time
	deficit := required - *tokens
	waitTime := time.Duration(deficit / float64(rate) * float64(time.Second))

	// Consume all available tokens
	*tokens = 0
	t.mu.Unlock()

	// Wait for the required time
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(waitTime):
		return nil
	}
}

// GetReadRate returns the current read throttle rate in bytes per second.
func (t *Throttler) GetReadRate() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.readRate
}

// GetWriteRate returns the current write throttle rate in bytes per second.
func (t *Throttler) GetWriteRate() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.writeRate
}

// GetBytesPerSecond returns the read throttle rate (for backward compatibility).
func (t *Throttler) GetBytesPerSecond() int64 {
	return t.GetReadRate()
}
