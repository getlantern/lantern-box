package adapter

import (
	"context"
	"net/http"
)

type directTransportKey struct{}

// ContextWithDirectTransport returns a copy of ctx carrying the given
// http.RoundTripper as the "direct transport". Outbounds that need to make
// HTTP requests outside the user-facing VPN tunnel (e.g. unbounded's signaling
// to freddie) should retrieve this transport via DirectTransportFromContext.
//
// The caller (typically radiance, when starting the libbox service) is
// responsible for building a RoundTripper that dials via the physical
// interface rather than re-entering the TUN — usually a kindling-backed
// RoundTripper whose underlying dialer is sing-box's direct outbound, which
// applies platform socket protection (VpnService.protect on Android,
// IP_BOUND_IF on iOS/macOS) automatically.
//
// If no direct transport is set, outbounds should fall back to their own
// DialerOptions-derived dialer. That path does NOT bypass the tunnel, so it
// is suitable for tests and stand-alone sing-box use but not for production
// on-device VPN operation.
func ContextWithDirectTransport(ctx context.Context, rt http.RoundTripper) context.Context {
	if rt == nil {
		return ctx
	}
	return context.WithValue(ctx, directTransportKey{}, rt)
}

// DirectTransportFromContext returns the direct http.RoundTripper stored in
// ctx, or nil if none was registered.
func DirectTransportFromContext(ctx context.Context) http.RoundTripper {
	rt, _ := ctx.Value(directTransportKey{}).(http.RoundTripper)
	return rt
}
