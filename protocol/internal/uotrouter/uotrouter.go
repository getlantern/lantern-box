// Package uotrouter wraps sing-box's UDP-over-TCP router so that route rules
// see the real destination of a UoT session rather than its magic address.
package uotrouter

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	boxuot "github.com/sagernet/sing-box/common/uot"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
)

// New returns a router that unwraps UoT connections before handing them to
// router. sing-box's uot.Router records the magic address in metadata.Domain
// after unwrapping, and domain rules prefer Domain over Destination.Fqdn, so
// without this every domain/domain_suffix rule would match the magic address
// instead of the host the client actually asked for.
func New(router adapter.ConnectionRouterEx, logger logger.ContextLogger) adapter.ConnectionRouterEx {
	return boxuot.NewRouter(realDomainRouter{router}, logger)
}

type realDomainRouter struct {
	adapter.ConnectionRouterEx
}

func (r realDomainRouter) RoutePacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	clearMagicDomain(&metadata)
	return r.ConnectionRouterEx.RoutePacketConnection(ctx, conn, metadata)
}

func (r realDomainRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	clearMagicDomain(&metadata)
	r.ConnectionRouterEx.RoutePacketConnectionEx(ctx, conn, metadata, onClose)
}

// clearMagicDomain leaves Domain empty so that rules fall back to
// Destination.Fqdn and sniffing or reverse DNS mapping can fill it in, exactly
// as for a UDP flow that arrived without UoT.
func clearMagicDomain(metadata *adapter.InboundContext) {
	if metadata.Domain == uot.MagicAddress || metadata.Domain == uot.LegacyMagicAddress {
		metadata.Domain = ""
	}
}
