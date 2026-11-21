package adapter

import "context"

// HedgeObserver learns which connections a hedge outbound sends through its
// First outbound. A host that wants to know registers one in the box's
// service context before the hedges are built. Every hedge in the box reports
// to it.
//
// FirstCommitted is called once per hedge for each connection that First's
// first reply commits to First. A hedge never moves a committed connection to
// Then, so all of the connection's traffic goes through First. hedge is the
// reporting hedge's tag and first is its First outbound's tag: when hedges
// are nested, an outer hedge whose First is another hedge reports too, so a
// host that counts traffic by path should check first. ctx is the context
// the connection was dialed with, so it carries what the router put there,
// such as the connection's log ID.
//
// The report is made before the reply that commits the connection is
// returned to the reader, so before any of First's replies reach the client.
// A reply that races the connection's close isn't reported.
//
// Connections that go to Then are never reported, and neither is one that
// stays on First but closes before First replies, such as a plaintext request
// that is never answered, so a host counting First's traffic undercounts it
// slightly.
//
// It is called on the connection's data path, so it must be fast and must
// not block.
type HedgeObserver interface {
	FirstCommitted(ctx context.Context, hedge, first string)
}
