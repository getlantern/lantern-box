// Package physnet keeps sockets that a VPN client opens for itself off the
// TUN it serves.
package physnet

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/service"
)

// ListenControl returns a net.ListenConfig.Control function that
// force-binds newly-created sockets to the platform's default physical
// interface. When no NetworkManager is registered on ctx (tests,
// standalone sing-box without the tunnel wrapper), it's a no-op — the
// socket follows the routing table like a plain net.ListenUDP.
//
// Implementation-wise we borrow the same ProtectFunc /
// AutoDetectInterfaceFunc hooks sing-box's default dialer uses, but we
// apply them UNCONDITIONALLY whenever a NetworkManager is present —
// not gated on networkManager.AutoDetectInterface() the way
// sing-box/common/dialer/default.go:NewDefault is. This is
// intentional: pion's ICE host-candidate gathering fails if sockets
// aren't kept off the TUN we're serving, even when the user hasn't
// explicitly set auto_detect_interface on the outbound. Gating on
// AutoDetectInterface() would reintroduce the "NAT failure, aborting!"
// regression the unbounded outbound's ListenUDP override exists to fix.
func ListenControl(ctx context.Context) control.Func {
	nm := service.FromContext[adapter.NetworkManager](ctx)
	if nm == nil {
		return nil
	}
	if pf := nm.ProtectFunc(); pf != nil {
		return pf
	}
	return nm.AutoDetectInterfaceFunc()
}
