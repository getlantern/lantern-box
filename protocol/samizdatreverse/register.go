// Package samizdatreverse implements reverse direct mode: the inviter's
// portal inbound accepts samizdat sessions that a helper's bridge inbound
// dials out, and the inviter's reverse outbounds open streams down those
// sessions, so the inviter's traffic exits from the helper.
package samizdatreverse

import (
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// RegisterInbounds registers the portal and bridge inbounds.
func RegisterInbounds(registry *inbound.Registry) {
	inbound.Register[option.SamizdatPortalInboundOptions](registry, constant.TypeSamizdatPortal, NewPortal)
	inbound.Register[option.SamizdatBridgeInboundOptions](registry, constant.TypeSamizdatBridge, NewBridge)
}

// RegisterOutbound registers the reverse outbound.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.SamizdatReverseOutboundOptions](registry, constant.TypeSamizdatReverse, NewReverseOutbound)
}
