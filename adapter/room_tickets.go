package adapter

import "context"

type roomTicketSourceKey struct{}

// ContextWithRoomTicketSource returns a copy of ctx carrying src, which returns
// the helper pairing tickets an Unbounded consumer presents to freddie to join
// its pairings' rooms (the inviter-role tickets lantern-cloud issues the
// device). The unbounded outbound calls src each time it subscribes for a
// volunteer, about every 20 seconds, so src must be cheap and must not block,
// and its result may change while the outbound runs: tickets live 15 minutes
// and the caller (radiance) renews them in the background.
//
// Tickets are credentials: neither src nor anything that reads it may log them.
// With no source, the outbound joins no rooms and uses the pool as before.
func ContextWithRoomTicketSource(ctx context.Context, src func() []string) context.Context {
	if src == nil {
		return ctx
	}
	return context.WithValue(ctx, roomTicketSourceKey{}, src)
}

// RoomTicketSourceFromContext returns the source stored in ctx, or nil.
func RoomTicketSourceFromContext(ctx context.Context) func() []string {
	src, _ := ctx.Value(roomTicketSourceKey{}).(func() []string)
	return src
}
