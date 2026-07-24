package adapter

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// ErrProxyRoutingLoop identifies a TUN request that targets the selected
// outbound's own physical endpoint.
var ErrProxyRoutingLoop = errors.New("proxy routing loop")

// ProxyRoutingLoopError reports which outbound would have recursively dialed
// itself. It deliberately omits the physical endpoint from the error.
type ProxyRoutingLoopError struct {
	OutboundTag string
}

func (e *ProxyRoutingLoopError) Error() string {
	if e == nil || e.OutboundTag == "" {
		return ErrProxyRoutingLoop.Error()
	}
	return fmt.Sprintf("%s: outbound %q", ErrProxyRoutingLoop, e.OutboundTag)
}

func (e *ProxyRoutingLoopError) Unwrap() error {
	return ErrProxyRoutingLoop
}

// PhysicalEndpointRegistry maps outbound tags to their resolved physical
// endpoints. Implementations must be safe for concurrent readers and writers.
type PhysicalEndpointRegistry interface {
	// Set atomically replaces tag's endpoint set. Invalid endpoints are ignored;
	// an empty resulting set is equivalent to Delete.
	Set(tag string, endpoints []netip.AddrPort)
	Contains(tag string, endpoint netip.AddrPort) bool
	Delete(tag string)
}

// NewPhysicalEndpointRegistry returns an empty in-memory registry.
func NewPhysicalEndpointRegistry() PhysicalEndpointRegistry {
	return &memoryPhysicalEndpointRegistry{
		endpoints: make(map[string]map[netip.AddrPort]struct{}),
	}
}

type memoryPhysicalEndpointRegistry struct {
	mu        sync.RWMutex
	endpoints map[string]map[netip.AddrPort]struct{}
}

func (r *memoryPhysicalEndpointRegistry) Set(tag string, endpoints []netip.AddrPort) {
	normalized := make(map[netip.AddrPort]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint, ok := normalizePhysicalEndpoint(endpoint); ok {
			normalized[endpoint] = struct{}{}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(normalized) == 0 {
		delete(r.endpoints, tag)
		return
	}
	r.endpoints[tag] = normalized
}

func (r *memoryPhysicalEndpointRegistry) Contains(tag string, endpoint netip.AddrPort) bool {
	endpoint, ok := normalizePhysicalEndpoint(endpoint)
	if !ok {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, found := r.endpoints[tag][endpoint]
	return found
}

func (r *memoryPhysicalEndpointRegistry) Delete(tag string) {
	r.mu.Lock()
	delete(r.endpoints, tag)
	r.mu.Unlock()
}

func normalizePhysicalEndpoint(endpoint netip.AddrPort) (netip.AddrPort, bool) {
	if !endpoint.IsValid() || endpoint.Port() == 0 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(endpoint.Addr().Unmap(), endpoint.Port()), true
}
