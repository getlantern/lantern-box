// Package samizdatreverse implements reverse direct mode: a helper's portal
// inbound accepts samizdat sessions an inviter's bridge inbound dials out,
// and the helper's reverse outbounds open streams back down those sessions.
package samizdatreverse

import (
	"context"
	"errors"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// RegisterInbounds registers the portal and bridge inbounds.
func RegisterInbounds(registry *inbound.Registry) {
	inbound.Register[option.SamizdatPortalInboundOptions](registry, constant.TypeSamizdatPortal, NewPortalInbound)
	inbound.Register[option.SamizdatBridgeInboundOptions](registry, constant.TypeSamizdatBridge, NewBridgeInbound)
}

// RegisterOutbound registers the reverse outbound.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.SamizdatReverseOutboundOptions](registry, constant.TypeSamizdatReverse, NewReverseOutbound)
}

// NewPortalInbound creates a portal inbound.
func NewPortalInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatPortalInboundOptions) (adapter.Inbound, error) {
	return nil, errors.New(constant.TypeSamizdatPortal + ": not implemented")
}

// NewBridgeInbound creates a bridge inbound.
func NewBridgeInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatBridgeInboundOptions) (adapter.Inbound, error) {
	return nil, errors.New(constant.TypeSamizdatBridge + ": not implemented")
}

// NewReverseOutbound creates a reverse outbound.
func NewReverseOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatReverseOutboundOptions) (adapter.Outbound, error) {
	return nil, errors.New(constant.TypeSamizdatReverse + ": not implemented")
}
