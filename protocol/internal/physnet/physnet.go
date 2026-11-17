// Package physnet keeps sockets that a VPN client opens for itself off the
// TUN it serves.
package physnet

import (
	"context"
	"errors"
	"syscall"

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
//
// Like that dialer it then sets route.default_mark, refusing it alongside
// tun.auto_redirect as the dialer does, and appends the tun's auto_redirect
// output mark, without which auto_redirect's nftables OUTPUT rules redirect
// the socket back into the tunnel on Linux. Both are read per socket, since
// the tun registers its mark at start, possibly after the caller got this
// func.
func ListenControl(ctx context.Context) control.Func {
	nm := service.FromContext[adapter.NetworkManager](ctx)
	if nm == nil {
		return nil
	}
	bind := nm.ProtectFunc()
	if bind == nil {
		bind = nm.AutoDetectInterfaceFunc()
	}
	return control.Append(bind, func(network, address string, conn syscall.RawConn) error {
		autoRedirect := nm.AutoRedirectOutputMark() != 0
		if mark := nm.DefaultOptions().RoutingMark; mark != 0 {
			if autoRedirect {
				return errDefaultMarkConflict
			}
			if err := applyMark(routingMark(mark), network, address, conn); err != nil {
				return err
			}
		}
		if !autoRedirect {
			return nil
		}
		return nm.AutoRedirectOutputMarkFunc()(network, address, conn)
	})
}

// errDefaultMarkConflict is the error sing-box's default dialer gives for the same sockets.
var errDefaultMarkConflict = errors.New("`route.default_mark` is conflict with `tun.auto_redirect`")

// routingMark is control.RoutingMark, which sets nothing outside Linux; tests replace it.
var routingMark = control.RoutingMark

func applyMark(f control.Func, network, address string, conn syscall.RawConn) error {
	if f == nil {
		return nil
	}
	return f(network, address, conn)
}
