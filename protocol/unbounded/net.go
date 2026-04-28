package unbounded

import (
	"context"
	"net"

	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

// rtcNet is a pion transport.Net backed by a sing-box N.Dialer. Every pion
// code path that reaches for a dial goes through this type so that detour,
// bind_interface, socket protection, and any other sing-box dialer plumbing
// apply uniformly — including from inside CreateDialer, which Nelson's earlier
// draft bypassed (that version called net.Dialer.Dial directly, skipping
// the sing-box dialer entirely).
type rtcNet struct {
	*stdnet.Net
	ctx    context.Context
	dialer N.Dialer
	logger log.ContextLogger
}

var _ transport.Net = (*rtcNet)(nil)

func newRTCNet(ctx context.Context, dialer N.Dialer, logger log.ContextLogger) (*rtcNet, error) {
	snet, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &rtcNet{
		Net:    snet,
		ctx:    ctx,
		dialer: dialer,
		logger: logger,
	}, nil
}

func (n *rtcNet) Dial(network, address string) (net.Conn, error) {
	return n.dialer.DialContext(n.ctx, network, metadata.ParseSocksaddr(address))
}

func (n *rtcNet) ListenPacket(network, address string) (net.PacketConn, error) {
	return n.dialer.ListenPacket(n.ctx, metadata.ParseSocksaddr(address))
}

// ListenUDP is the path pion's ICE agent takes for every host candidate
// (see pion/ice/v4 gather.go → listenUDPInPortRange → a.net.ListenUDP).
// Without this override, the call would fall through to the embedded
// *stdnet.Net, which is a thin wrapper around net.ListenUDP — that
// creates sockets with no platform socket-protection, so on a VPN
// client where the host process also serves the default TUN, the
// sockets' egress packets follow the routing table straight back
// through the TUN. ICE connectivity checks never reach the peer and
// every session dies with "NAT failure, aborting!" after 5s.
//
// We can't funnel this through n.dialer.ListenPacket because that API
// doesn't take a local bind address (its signature is destination-only),
// and pion needs one socket per interface IP to gather per-interface
// host candidates. So we construct a net.ListenConfig directly and
// attach the NetworkManager's bind-to-physical-interface Control
// function (the same one sing-box's DefaultDialer appends when
// auto_detect_interface is set at the route level — via ProtectFunc
// on platforms with a platformInterface, or AutoDetectInterfaceFunc
// otherwise). Sockets still bind to the per-interface local address
// pion requested, but their egress interface is force-bound via
// IP_BOUND_IF (macOS/iOS) / SO_BINDTODEVICE (Linux/Android).
func (n *rtcNet) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	lc := net.ListenConfig{
		Control: bindEgressToPhysicalInterface(n.ctx),
	}
	addr := ""
	if laddr != nil {
		addr = laddr.String()
	}
	pc, err := lc.ListenPacket(n.ctx, network, addr)
	if err != nil {
		return nil, err
	}
	udpConn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, &net.OpError{
			Op:  "listen",
			Net: network,
			Err: net.InvalidAddrError("ListenConfig did not return *net.UDPConn"),
		}
	}
	return udpConn, nil
}

// bindEgressToPhysicalInterface returns a net.ListenConfig.Control
// function that force-binds newly-created sockets to the platform's
// default physical interface. When no NetworkManager is registered on
// ctx (tests, standalone sing-box without the tunnel wrapper), it's a
// no-op — the socket follows the routing table like a plain
// net.ListenUDP.
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
// regression this override exists to fix.
func bindEgressToPhysicalInterface(ctx context.Context) control.Func {
	nm := service.FromContext[adapter.NetworkManager](ctx)
	if nm == nil {
		return nil
	}
	if pf := nm.ProtectFunc(); pf != nil {
		return pf
	}
	return nm.AutoDetectInterfaceFunc()
}

func (n *rtcNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	conn, err := n.dialer.DialContext(n.ctx, network, metadata.SocksaddrFromNet(raddr))
	if err != nil {
		return nil, err
	}
	tcpConn, ok := conn.(transport.TCPConn)
	if !ok {
		conn.Close()
		return nil, &net.OpError{Op: "dial", Net: network, Addr: raddr,
			Err: net.InvalidAddrError("sing-box dialer returned non-TCP conn")}
	}
	return tcpConn, nil
}

func (n *rtcNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.dialer.DialContext(n.ctx, network, metadata.SocksaddrFromNet(raddr))
	if err != nil {
		return nil, err
	}
	udpConn, ok := conn.(transport.UDPConn)
	if !ok {
		conn.Close()
		return nil, &net.OpError{Op: "dial", Net: network, Addr: raddr,
			Err: net.InvalidAddrError("sing-box dialer returned non-UDP conn")}
	}
	return udpConn, nil
}

// CreateDialer returns a transport.Dialer whose every method funnels back
// through rtcNet (not the given *net.Dialer). This is the fix for the bypass
// in Nelson's earlier draft — pion's webrtc agent calls CreateDialer and then
// uses that dialer for candidate dials; routing those through net.Dialer
// would skip sing-box's bind_interface / detour / socket-protection plumbing.
func (n *rtcNet) CreateDialer(_ *net.Dialer) transport.Dialer {
	return &rtcDialer{net: n}
}

type rtcDialer struct {
	net *rtcNet
}

func (d *rtcDialer) Dial(network, address string) (net.Conn, error) {
	return d.net.Dial(network, address)
}

// The Resolve* methods fall through to the embedded stdnet.Net, which resolves
// via the OS resolver. On mobile that's the physical-interface resolver, not
// the TUN's. Kept as passthroughs so pion can enumerate local interfaces for
// ICE candidate gathering; routing DNS through the sing-box dialer here would
// not make sense (it's not a dial, and we don't want to add sing-box's DNS
// policy to pion's internal bookkeeping).
