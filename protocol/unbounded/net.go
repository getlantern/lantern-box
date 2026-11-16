package unbounded

import (
	"context"
	"net"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/protocol/internal/physnet"
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

// myInterfaces is the set of interfaces the VPN serving n.ctx registered as its own: its tunnel. It
// is read on each call, since the tunnel comes back under a new name after a restart, and is nil
// with no VPN.
func (n *rtcNet) myInterfaces() map[string]bool {
	nm := service.FromContext[adapter.NetworkManager](n.ctx)
	if nm == nil {
		return nil
	}
	monitor := nm.InterfaceMonitor()
	if monitor == nil {
		return nil
	}
	names := monitor.MyInterfaces()
	if len(names) == 0 {
		return nil
	}
	mine := make(map[string]bool, len(names))
	for _, name := range names {
		mine[name] = true
	}
	return mine
}

// Interfaces lists the host's interfaces for pion's host candidates, without the VPN's own. A
// candidate on the tunnel's address sends into the tunnel, even from this process whose other
// sockets skip it, and physnet would pin its socket there: its checks then reach whichever outbound
// the tunnel routes to, which for a personal proxy is the very helper they are trying to reach.
// Like limitedNet's, the list is fresh each call, since this Net outlives network changes.
func (n *rtcNet) Interfaces() ([]*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	ifs, err := fresh.Interfaces()
	if err != nil {
		return nil, err
	}
	mine := n.myInterfaces()
	if mine == nil {
		return ifs, nil
	}
	kept := make([]*transport.Interface, 0, len(ifs))
	for _, i := range ifs {
		if !mine[i.Name] {
			kept = append(kept, i)
		}
	}
	return kept, nil
}

// InterfaceByIndex is Interfaces' lookup by index: the VPN's own interfaces aren't found.
func (n *rtcNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	i, err := fresh.InterfaceByIndex(index)
	if err != nil {
		return nil, err
	}
	if n.myInterfaces()[i.Name] {
		return nil, transport.ErrInterfaceNotFound
	}
	return i, nil
}

// InterfaceByName is Interfaces' lookup by name: the VPN's own interfaces aren't found.
func (n *rtcNet) InterfaceByName(name string) (*transport.Interface, error) {
	if n.myInterfaces()[name] {
		return nil, transport.ErrInterfaceNotFound
	}
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	return fresh.InterfaceByName(name)
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
		Control: physnet.ListenControl(n.ctx),
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
