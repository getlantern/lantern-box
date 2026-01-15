package datacap

import (
	"sync"
)

// ThrottleRegistry manages shared throttlers per device.
// All connections from the same device share a single Throttler instance,
// ensuring the bandwidth limit is enforced across all parallel connections.
type ThrottleRegistry struct {
	mu         sync.RWMutex
	throttlers map[string]*Throttler
}

// NewThrottleRegistry creates a new registry.
func NewThrottleRegistry() *ThrottleRegistry {
	return &ThrottleRegistry{
		throttlers: make(map[string]*Throttler),
	}
}

// GetOrCreate returns the throttler for the given device ID.
// If no throttler exists for the device, a new one is created with throttling disabled.
// Throttling is enabled later via UpdateRates when the datacap status indicates throttle=true.
func (r *ThrottleRegistry) GetOrCreate(deviceID string) *Throttler {
	// Check if throttler exists
	r.mu.RLock()
	throttler, exists := r.throttlers[deviceID]
	r.mu.RUnlock()

	if exists {
		return throttler
	}

	// Create new throttler
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if throttler, exists = r.throttlers[deviceID]; exists {
		return throttler
	}

	// Create new throttler with throttling disabled by default
	// It will be enabled when the sidecar reports throttle=true
	throttler = NewThrottler(0)
	r.throttlers[deviceID] = throttler
	return throttler
}

// Count returns the number of devices with throttlers.
// Useful for testing and monitoring.
func (r *ThrottleRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.throttlers)
}
