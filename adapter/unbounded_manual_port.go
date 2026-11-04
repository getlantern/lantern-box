package adapter

import "context"

type unboundedManualPortKey struct{}

// ContextWithUnboundedManualPort returns a copy of ctx carrying port, a port the
// user forwarded by hand on their router (radiance's peer_manual_port). Unbounded
// outbounds then carry their WebRTC traffic on that UDP port and advertise it to
// helpers instead of asking the gateway for a mapping. The user is expected to have
// forwarded UDP as well as TCP on it. A zero port registers nothing.
func ContextWithUnboundedManualPort(ctx context.Context, port uint16) context.Context {
	if port == 0 {
		return ctx
	}
	return context.WithValue(ctx, unboundedManualPortKey{}, port)
}

// UnboundedManualPortFromContext returns the port stored in ctx, or 0.
func UnboundedManualPortFromContext(ctx context.Context) uint16 {
	port, _ := ctx.Value(unboundedManualPortKey{}).(uint16)
	return port
}
