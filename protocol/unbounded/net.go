package unbounded

import (
	"context"
	"net"

	"github.com/pion/transport/v3"
	"github.com/pion/transport/v3/stdnet"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
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
