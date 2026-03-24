package adapter

import (
	"context"
	"net/http"
)

type directTransportKey struct{}

// ContextWithDirectTransport returns a copy of ctx carrying the given
// http.RoundTripper as the "direct transport". Outbounds that need to make
// HTTP requests that bypass the VPN tunnel (e.g., unbounded's signaling)
// should retrieve this transport via DirectTransportFromContext.
//
// The caller (typically radiance) is responsible for building a RoundTripper
// whose DialContext uses the sing-box direct outbound, ensuring that socket
// protection (VpnService.protect on Android, IP_BOUND_IF on iOS/macOS, etc.)
// is applied automatically.
func ContextWithDirectTransport(ctx context.Context, rt http.RoundTripper) context.Context {
	return context.WithValue(ctx, directTransportKey{}, rt)
}

// DirectTransportFromContext returns the direct http.RoundTripper stored in
// ctx, or nil if none was registered.
func DirectTransportFromContext(ctx context.Context) http.RoundTripper {
	rt, _ := ctx.Value(directTransportKey{}).(http.RoundTripper)
	return rt
}
