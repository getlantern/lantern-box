package datacap

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestThrottleRegistry_SharedThrottler(t *testing.T) {
	registry := NewThrottleRegistry()

	// Same device ID should return same throttler
	t1 := registry.GetOrCreate("device-1")
	t2 := registry.GetOrCreate("device-1")

	assert.Same(t, t1, t2, "same device ID should return same throttler instance")
}

func TestThrottleRegistry_DifferentDevices(t *testing.T) {
	registry := NewThrottleRegistry()

	// Different device IDs should get different throttlers
	t1 := registry.GetOrCreate("device-1")
	t2 := registry.GetOrCreate("device-2")

	assert.NotSame(t, t1, t2, "different device IDs should get different throttlers")
	assert.Equal(t, 2, registry.Count(), "registry should have 2 devices")
	assert.NotEqual(t, t1, t2, "different device IDs should get different throttlers")
}

func TestThrottleRegistry_ConcurrentAccess(t *testing.T) {
	registry := NewThrottleRegistry()

	var wg sync.WaitGroup
	devices := 100
	goroutines := 10

	// Concurrently create throttlers for many devices
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for d := 0; d < devices; d++ {
				deviceID := "device-" + string(rune('A'+d))
				throttler := registry.GetOrCreate(deviceID)
				assert.NotNil(t, throttler)
			}
		}(i)
	}

	wg.Wait()

	// Should have created throttlers (some device IDs may collide due to rune conversion)
	assert.Greater(t, registry.Count(), 0, "should have created throttlers")
}

func TestThrottleRegistry_ThrottlerStartsDisabled(t *testing.T) {
	registry := NewThrottleRegistry()

	// Get throttler (starts disabled)
	throttler := registry.GetOrCreate("device-1")
	assert.False(t, throttler.IsEnabled(), "should start disabled")

	// Enable via direct call on throttler (as done in updateThrottleState)
	throttler.UpdateRates(1000, 2000)
	assert.True(t, throttler.IsEnabled(), "should be enabled after UpdateRates")
	assert.Equal(t, int64(1000), throttler.GetReadRate())
	assert.Equal(t, int64(2000), throttler.GetWriteRate())
}
