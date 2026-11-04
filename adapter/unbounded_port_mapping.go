package adapter

import "context"

type unboundedPortMappingKey struct{}

// ContextWithUnboundedPortMapping returns a copy of ctx that has Unbounded
// outbounds carry their WebRTC traffic on one UDP port forwarded by the user's
// router, advertised to helpers as an extra candidate. manualPort is a port the
// user forwarded by hand (radiance's peer_manual_port), for UDP as well as TCP;
// 0 asks the gateway for a mapping by UPnP, PCP or NAT-PMP.
//
// Without it, outbounds map nothing. Mapping changes the user's router, so the
// embedder opts in, and tests that start an outbound don't touch the
// developer's router.
func ContextWithUnboundedPortMapping(ctx context.Context, manualPort uint16) context.Context {
	return context.WithValue(ctx, unboundedPortMappingKey{}, manualPort)
}

// UnboundedPortMappingFromContext reports whether ctx enables port mapping, and
// with which manual port (0 for automatic).
func UnboundedPortMappingFromContext(ctx context.Context) (manualPort uint16, enabled bool) {
	manualPort, enabled = ctx.Value(unboundedPortMappingKey{}).(uint16)
	return manualPort, enabled
}
