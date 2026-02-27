package protocol

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/protocol/algeneva"
	"github.com/getlantern/lantern-box/protocol/amnezia"
	"github.com/getlantern/lantern-box/protocol/group"
	"github.com/getlantern/lantern-box/protocol/outline"
	"github.com/getlantern/lantern-box/protocol/samizdat"
	"github.com/getlantern/lantern-box/protocol/unbounded"
	"github.com/getlantern/lantern-box/protocol/water"
)

var supportedProtocols = []string{
	// custom protocols
	"algeneva",
	"amnezia",
	"outline",
	"samizdat",

	// sing-box built-in protocols
	"http",
	"hysteria",
	"hysteria2",
	"shadowsocks",
	"shadowtls",
	"socks",
	"ssh",
	"tor",
	"trojan",
	"tuic",
	"vless",
	"vmess",
	"wireguard",
}

// RegisterProtocols registers all lantern-box protocols to the given context's registries.
// Note: this does not register sing-box built-in protocols.
func RegisterProtocols(ctx context.Context) context.Context {
	if registry := service.FromContext[adapter.InboundRegistry](ctx); registry != nil {
		if reg, ok := registry.(*inbound.Registry); ok {
			registerInbounds(reg)
		}
	}
	if registry := service.FromContext[adapter.OutboundRegistry](ctx); registry != nil {
		if reg, ok := registry.(*outbound.Registry); ok {
			registerOutbounds(reg)
		}
	}
	if registry := service.FromContext[adapter.EndpointRegistry](ctx); registry != nil {
		if reg, ok := registry.(*endpoint.Registry); ok {
			registerEndpoints(reg)
		}
	}
	return ctx
}

// ***** REGISTER NEW PROTOCOLS HERE ***** //

func registerInbounds(registry *inbound.Registry) {
	algeneva.RegisterInbound(registry)
	samizdat.RegisterInbound(registry)
	water.RegisterInbound(registry)
}

func registerOutbounds(registry *outbound.Registry) {
	// custom protocol outbounds
	algeneva.RegisterOutbound(registry)
	outline.RegisterOutbound(registry)
	samizdat.RegisterOutbound(registry)
	water.RegisterOutbound(registry)
	unbounded.RegisterOutbound(registry)

	// utility outbounds
	group.RegisterFallback(registry)
	group.RegisterMutableSelector(registry)
	group.RegisterMutableURLTest(registry)
}

func registerEndpoints(registry *endpoint.Registry) {
	amnezia.RegisterEndpoint(registry)
}

func SupportedProtocols() []string {
	return supportedProtocols
}
