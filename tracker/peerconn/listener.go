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

// Listener receives connection lifecycle notifications from lantern-box
// inbound protocols.
//
//   state  +1 on accept, -1 on close
//   source remote peer's "ip:port" string (empty if unavailable)
//
// The listener is invoked synchronously on the inbound's accept/close path,
// so implementations should not block on heavy work — fan out to a buffered
// channel or goroutine if more than a few microseconds is expected.
type Listener func(state int, source string)

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

// Notify dispatches a state transition to the registered listener if one
// is set. No-op when nothing is registered, so lantern-box uses that don't
// care about peer-share connection events (cmd_run, the standalone CLI,
// the radiance VPN client) pay zero cost beyond a mutex read.
func Notify(state int, source string) {
	listenerMu.RLock()
	l := listener
	listenerMu.RUnlock()
	if l != nil {
		l(state, source)
	}
}
