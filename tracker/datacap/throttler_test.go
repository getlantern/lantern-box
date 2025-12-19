package datacap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestThrottler_ZeroOrNegativeRates(t *testing.T) {
	ctx := context.Background()

	t.Run("zero rate", func(t *testing.T) {
		throttler := NewThrottler(0)
		err := throttler.WaitRead(ctx, 100)
		assert.NoError(t, err)
	})

	t.Run("negative rate", func(t *testing.T) {
		throttler := NewThrottler(-100)
		err := throttler.WaitRead(ctx, 100)
		assert.NoError(t, err)
	})
}

func TestThrottler_ContextCancellation(t *testing.T) {
	// create a throttler with a very slow rate
	throttler := NewThrottler(10) // 10 bytes/sec

	// consume initial tokens
	_ = throttler.WaitRead(context.Background(), 10)

	// try to consume more, which should block
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := throttler.WaitRead(ctx, 100)

	duration := time.Since(start)

	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
	assert.Less(t, duration, 2*time.Second, "Should have cancelled quickly")
}

func TestThrottler_ConcurrentAccess(t *testing.T) {
	rate := int64(1024 * 1024) // 1MB/s
	throttler := NewThrottler(rate)
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 10
	iterations := 100

	// Start concurrent readers and writers
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				err := throttler.WaitRead(ctx, 10)
				assert.NoError(t, err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				err := throttler.WaitWrite(ctx, 10)
				assert.NoError(t, err)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out - potential deadlock")
	}
}

func TestThrottler_TokenRefill(t *testing.T) {
	// Rate: 100 bytes/sec
	rate := int64(100)
	throttler := NewThrottler(rate)
	ctx := context.Background()

	// Initial state: bucket full (100 tokens)
	// Consume 100 bytes - should be immediate
	start := time.Now()
	err := throttler.WaitRead(ctx, 100)
	assert.NoError(t, err)
	assert.WithinDuration(t, start, time.Now(), 10*time.Millisecond)

	// Bucket empty now.
	// Consume 50 bytes.
	// We need 50 tokens. At 100 bytes/sec, that takes 0.5 seconds.
	start = time.Now()
	err = throttler.WaitRead(ctx, 50)
	assert.NoError(t, err)

	elapsed := time.Since(start)
	// Allow small margin of error for scheduler
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(450), "Should wait at least ~0.5s")
}

func TestThrottler_SeparateRates(t *testing.T) {
	readRate := int64(1000)
	writeRate := int64(10) // Very slow write

	throttler := NewThrottlerWithRates(readRate, writeRate)
	ctx := context.Background()

	// Read should be fast
	start := time.Now()
	err := throttler.WaitRead(ctx, 100)
	assert.NoError(t, err)
	assert.Less(t, time.Since(start), 100*time.Millisecond)

	// Drain write bucket
	_ = throttler.WaitWrite(ctx, 10)

	// Write should be slow (needs 1s for 10 bytes)
	start = time.Now()
	err = throttler.WaitWrite(ctx, 10)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start).Milliseconds(), int64(900))
}

func TestThrottler_Disable(t *testing.T) {
	throttler := NewThrottler(10) // Slow rate
	ctx := context.Background()

	// Prove it's slow first
	_ = throttler.WaitRead(ctx, 10) // Drain
	start := time.Now()
	_ = throttler.WaitRead(ctx, 10) // Wait 1s
	assert.GreaterOrEqual(t, time.Since(start).Milliseconds(), int64(900))

	// Disable it
	throttler.Disable()
	assert.False(t, throttler.IsEnabled())

	// Should be instant now
	start = time.Now()
	err := throttler.WaitRead(ctx, 1000)
	assert.NoError(t, err)
	assert.Less(t, time.Since(start), 10*time.Millisecond)
}

func TestThrottler_LargeRead(t *testing.T) {
	// Rate: 100 bytes/sec
	throttler := NewThrottler(100)
	ctx := context.Background()

	// Consume initial full bucket (100 tokens)
	_ = throttler.WaitRead(ctx, 100)

	// rate is 100B/s, so reading 300B requires 3s wait
	start := time.Now()
	err := throttler.WaitRead(ctx, 300)
	assert.NoError(t, err)

	elapsed := time.Since(start)
	// Expect ~3 seconds
	assert.InDelta(t, float64(3000), float64(elapsed.Milliseconds()), 200, "Should wait approximately 3 seconds for large read")
}

func TestThrottler_EnableDisableRapidly(t *testing.T) {
	throttler := NewThrottler(10)
	ctx := context.Background()

	for i := 0; i < 1000; i++ {
		throttler.Disable()
		assert.False(t, throttler.IsEnabled())
		err := throttler.WaitRead(ctx, 100)
		assert.NoError(t, err) // Should be instant

		throttler.Enable(10)
		assert.True(t, throttler.IsEnabled())
		// Don't wait here otherwise test takes forever, just verify state consistency
	}
}

func TestThrottler_ZeroWait(t *testing.T) {
	throttler := NewThrottler(100)
	err := throttler.WaitRead(context.Background(), 0)
	assert.NoError(t, err)
	// Should do nothing and return instantly
}

func TestThrottler_WaitPrecision(t *testing.T) {
	rate := int64(1000) // 1000 bytes/s = 1 byte/ms
	throttler := NewThrottler(rate)
	ctx := context.Background()

	// Drain bucket
	_ = throttler.WaitRead(ctx, 1000)

	// Wait for 500 bytes (should take 500ms)
	start := time.Now()
	err := throttler.WaitRead(ctx, 500)
	assert.NoError(t, err)

	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(450))
	assert.Less(t, elapsed.Milliseconds(), int64(600))
}

func TestThrottler_RefillCap(t *testing.T) {
	// Rate 1000. Bucket capacity 1000.
	throttler := NewThrottler(1000)

	// Wait 2 seconds. Tokens should not exceed capacity (1000).
	time.Sleep(2 * time.Second)

	// consume 1500 bytes. if the bucket grew unbounded to 2000+ tokens (2s * 1000B/s),
	// this would return instantly. since it's capped at capacity (1000),
	// we should drain 1000 and wait for the remaining 500 (~0.5s).

	start := time.Now()
	err := throttler.WaitRead(context.Background(), 1500)
	assert.NoError(t, err)

	elapsed := time.Since(start)
	// Should wait ~0.5s. If it waited 0, then refill cap logic is broken.
	assert.Greater(t, elapsed.Milliseconds(), int64(400), "Should have capped token refill")
}
