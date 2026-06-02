package protocol

import (
	"context"
	"maps"
	"slices"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/protocol/algeneva"
	"github.com/getlantern/lantern-box/protocol/amnezia"
	"github.com/getlantern/lantern-box/protocol/group"
	"github.com/getlantern/lantern-box/protocol/outline"
	"github.com/getlantern/lantern-box/protocol/reflex"
	"github.com/getlantern/lantern-box/protocol/samizdat"
	"github.com/getlantern/lantern-box/protocol/unbounded"
	"github.com/getlantern/lantern-box/protocol/water"
)

var supportedProtocols []string

func init() {
	// collect supported protocols from all registries. since what's supported depends on the build
	// tags, we can't hardcode this list and must collect it from the registries themselves.

	ctx := RegisterProtocols(libbox.BaseContext(nil))
	getProtos := func(reg adapter.Registry) []string {
		if reg == nil {
			return nil
		}
		return reg.Registered()
	}

	iprotos := getProtos(service.FromContext[adapter.InboundRegistry](ctx))
	oprotos := getProtos(service.FromContext[adapter.OutboundRegistry](ctx))
	eprotos := getProtos(service.FromContext[adapter.EndpointRegistry](ctx))
	protocolSet := make(map[string]struct{})
	for _, p := range iprotos {
		protocolSet[p] = struct{}{}
	}
	for _, p := range oprotos {
		protocolSet[p] = struct{}{}
	}
	for _, p := range eprotos {
		protocolSet[p] = struct{}{}
	}
	if !with_wireguard {
		delete(protocolSet, constant.TypeAmnezia)
		delete(protocolSet, C.TypeWireGuard)
	}
	if !with_ts {
		delete(protocolSet, C.TypeTailscale)
	}
	supportedProtocols = slices.Collect(maps.Keys(protocolSet))
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
	reflex.RegisterInbound(registry)
	samizdat.RegisterInbound(registry)
	water.RegisterInbound(registry)
}

func registerOutbounds(registry *outbound.Registry) {
	// custom protocol outbounds
	algeneva.RegisterOutbound(registry)
	outline.RegisterOutbound(registry)
	reflex.RegisterOutbound(registry)
	samizdat.RegisterOutbound(registry)
	unbounded.RegisterOutbound(registry)
	water.RegisterOutbound(registry)

	// utility outbounds
	group.RegisterFallback(registry)
	group.RegisterMutableSelector(registry)
	group.RegisterMutableURLTest(registry)
	group.RegisterMutableAutoSelect(registry)
}

func registerEndpoints(registry *endpoint.Registry) {
	amnezia.RegisterEndpoint(registry)
}

func SupportedProtocols() []string {
	return supportedProtocols
}
