// Package peerconn exposes a process-wide listener registry for inbound
// connection lifecycle events across lantern-box protocols.
//
// This is the hook the radiance peer client (Share My Connection) uses to
// emit per-connection accept/close events to the rest of the application
// — the globe visualization, abuse-detection aggregation, and metrics that
// need a stream rather than a snapshot. It deliberately sits outside any
// individual protocol package so the same listener fires for samizdat
// today and TLS-masq / algeneva / water as those inbounds gain peer-share
// support.
//
// Why not adapter.ConnectionTracker? sing-box's tracker abstraction lives
// behind libbox's internal box.Router, with no public hook for callers
// constructing a libbox.BoxService to AppendTracker post-creation. A
// process-wide listener side-steps that: lantern-box inbound code calls
// Notify directly, the radiance peer client registers a listener before
// starting libbox.
package peerconn

import "sync"

// Event describes a single connection lifecycle transition surfaced from
// a lantern-box inbound to the peer-share consumer (radiance peer client).
//
//   State        +1 on accept, -1 on close
//   Source       remote peer's "ip:port" string (empty if unavailable)
//   Destination  the host:port the remote peer requested through the
//                inbound (only meaningful on +1; close events leave it
//                empty since the abuse aggregator already pairs each
//                close with the prior accept by source identity)
//
// Destination carries the load-bearing abuse-detection signal: source
// IP alone is insufficient (mobile clients change IPs, NAT pools
// collapse to one address) but the destination distribution per source
// bucket is highly diagnostic — one source making thousands of port-25
// connections is a spam relay; one source making millions of HTTPS
// connections to a fixed e-commerce list is credential stuffing.
//
// Bucket aggregation lives outside this package (see radiance/peer);
// the registry just hands raw events off to whoever's listening.
type Event struct {
	State       int
	Source      string
	Destination string
}

// Listener receives connection lifecycle notifications from lantern-box
// inbound protocols. Invoked synchronously on the inbound's accept/close
// path, so implementations should not block on heavy work — fan out to
// a buffered channel or goroutine if more than a few microseconds is
// expected.
type Listener func(evt Event)

var (
	listenerMu sync.RWMutex
	listener   Listener
)

// SetListener registers (or with a nil argument, unregisters) the global
// listener. Single-active: a second SetListener call replaces the first.
// Called from the radiance peer client at Start, cleared at Stop.
func SetListener(l Listener) {
	listenerMu.Lock()
	defer listenerMu.Unlock()
	listener = l
}

// Notify dispatches an event to the registered listener if one is set.
// No-op when nothing is registered — non-peer-share consumers (cmd_run,
// the standalone CLI, the radiance VPN client) pay zero cost beyond a
// mutex read. Lower-level entry exposed for future hooks (byte-counting
// conn wrappers, periodic flush emitters); accept/close call sites
// should prefer NotifyAccept / NotifyClose.
func Notify(evt Event) {
	listenerMu.RLock()
	l := listener
	listenerMu.RUnlock()
	if l != nil {
		l(evt)
	}
}

// NotifyAccept is the convenience caller for inbound accept paths.
func NotifyAccept(source, destination string) {
	Notify(Event{State: +1, Source: source, Destination: destination})
}

// NotifyClose is the convenience caller for inbound close paths.
func NotifyClose(source string) {
	Notify(Event{State: -1, Source: source})
}
