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

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Counter is a connection's running byte totals, from the peer's side: Down
// is sent to the remote client, Up is received from it. The inbound adds to it
// as bytes move, so a listener holding it can read live totals while the
// connection is open, and the final totals once the close event arrives.
type Counter struct {
	down, up atomic.Uint64
}

// Down returns the bytes sent to the remote client so far.
func (c *Counter) Down() uint64 { return c.down.Load() }

// Up returns the bytes received from the remote client so far.
func (c *Counter) Up() uint64 { return c.up.Load() }

// AddDown records n bytes sent to the remote client.
func (c *Counter) AddDown(n int) {
	if n > 0 {
		c.down.Add(uint64(n))
	}
}

// AddUp records n bytes received from the remote client.
func (c *Counter) AddUp(n int) {
	if n > 0 {
		c.up.Add(uint64(n))
	}
}

// Event describes a single connection lifecycle transition surfaced from
// a lantern-box inbound to the peer-share consumer (radiance peer client).
//
//	State        +1 on accept, -1 on close
//	Source       remote peer's "ip:port" string (empty if unavailable)
//	Destination  the host:port the remote peer requested through the
//	             inbound. Always set on +1. Close events leave it empty,
//	             since the abuse aggregator already pairs each close with
//	             the prior accept by source identity, except when Rejected
//	             is set.
//	Rejected     on -1 only: the router refused the destination by a
//	             reject rule (the peer's allowlist or an abuse block), as
//	             opposed to the connection failing or ending normally.
//	             Destination is repeated so a consumer can tally refusals
//	             without keeping per-connection state.
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
	Rejected    bool
	// Bytes is the connection's running byte totals: the same Counter on
	// its accept and its close, or nil if the inbound does not count bytes.
	Bytes *Counter
	// ShortID is the samizdat short ID the connection's session authenticated
	// with, the same on its accept and its close, or zero if the inbound has
	// none. Each personal-proxy pairing gets its own short ID, so it tells a
	// listener which pairing a connection came through. A short ID identifies
	// a friend relationship: it must never be logged, traced, or put in
	// metrics, and String and LogValue leave it out.
	ShortID [8]byte
}

// String formats evt without its ShortID, so %v and %+v of an Event never
// print it.
func (evt Event) String() string {
	return fmt.Sprintf("{State:%d Source:%s Destination:%s Rejected:%t}",
		evt.State, evt.Source, evt.Destination, evt.Rejected)
}

// GoString is String, so %#v leaves ShortID out too.
func (evt Event) GoString() string { return "peerconn.Event" + evt.String() }

// LogValue leaves out ShortID, so slog never records it.
func (evt Event) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("state", evt.State),
		slog.String("source", evt.Source),
		slog.String("destination", evt.Destination),
		slog.Bool("rejected", evt.Rejected),
	)
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

// Notify dispatches a single event to the registered listener if one is set.
// No-op when nothing is registered — non-peer-share consumers (cmd_run, the
// standalone CLI, the radiance VPN client) pay zero cost beyond a mutex read.
// Lower-level entry exposed for future one-shot hooks (byte-counting conn
// wrappers, periodic flush emitters).
//
// Callers emitting both halves of one connection's lifecycle must use Acquire
// instead, so the pair cannot be split across two listeners.
func Notify(evt Event) {
	listenerMu.RLock()
	l := listener
	listenerMu.RUnlock()
	if l != nil {
		l(evt)
	}
}

// Acquire returns the registered listener, or nil if none is set.
//
// A connection emitting a matched accept/close pair must Acquire once and use
// the returned listener for both halves. Reading the registry twice lets a
// SetListener land in between, so the accept reaches one listener and the
// close reaches another: the first is left holding a connection that never
// closes, the second sees a close it never opened. Consumers keeping
// per-connection state — counts, open-connection maps, abuse buckets keyed on
// the accept — corrupt silently when that happens.
//
// The tradeoff is that a pair already in flight completes against the
// listener that accepted it, so SetListener(nil) does not cancel the close
// half. Consumers needing a hard stop must gate inside their own callback.
func Acquire() Listener {
	listenerMu.RLock()
	defer listenerMu.RUnlock()
	return listener
}
