//go:build !with_quic

package hysteria2x

import "github.com/sagernet/sing-box/adapter/outbound"

// RegisterOutbound is a no-op without QUIC support, so the protocol is not
// advertised in supportedProtocols.
func RegisterOutbound(registry *outbound.Registry) {}
