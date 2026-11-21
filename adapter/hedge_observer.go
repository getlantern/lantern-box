package adapter

import "context"

// HedgeObserver learns which connections the hedge outbound sends through
// its First outbound. A host that wants to know registers one in the box's
// service context before the hedge is built.
//
// FirstCommitted is called once for each connection that First's first reply
// commits to First. The hedge never moves a committed connection to Then, so
// all of its traffic goes through First. ctx is the context the connection
// was dialed with, so it carries what the router put there, such as the
// connection's log ID. A connection that goes to Then, or that closes before
// First replies, is never reported.
//
// It is called on the connection's data path, so it must be fast and must
// not block.
type HedgeObserver interface {
	FirstCommitted(ctx context.Context)
}
