package unbounded

import (
	"context"
	"net"

	"github.com/pion/transport/v3"
	"github.com/pion/transport/v3/stdnet"

	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

var _ transport.Net = (*rtcNet)(nil)

// rtcNet is a [transport.Net] implementation that uses a N.Dialer for dialing.
// It wraps the base [stdnet.Net] and overrides the Dial and ListenPacket methods
// to use the DefaultDialer.
type rtcNet struct {
	*stdnet.Net
	ctx    context.Context
	dialer N.Dialer
}

func newRTCNet(ctx context.Context, dialer N.Dialer) (*rtcNet, error) {
	snet, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &rtcNet{
		Net:    snet,
		ctx:    ctx,
		dialer: dialer,
	}, nil
}

func (n *rtcNet) Dial(network string, address string) (net.Conn, error) {
	destination := metadata.ParseSocksaddr(address)
	return n.dialer.DialContext(n.ctx, network, destination)
}

func (n *rtcNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	destination := metadata.ParseSocksaddr(address)
	return n.dialer.ListenPacket(n.ctx, destination)
}

func (n *rtcNet) DialUDP(network string, laddr *net.UDPAddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	destination := metadata.SocksaddrFromNet(raddr)
	conn, err := n.dialer.DialContext(n.ctx, "udp", destination)
	if err != nil {
		return nil, err
	}
	// the DefaultDialer should always return a net.UDPConn
	if udpConn, ok := conn.(transport.UDPConn); ok {
		return udpConn, nil
	}
	conn.Close()
	return nil, &net.OpError{
		Op:   "dial",
		Net:  network,
		Addr: raddr,
		Err:  net.InvalidAddrError("not a UDP connection"),
	}
}

func (n *rtcNet) DialTCP(network string, laddr *net.TCPAddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	destination := metadata.SocksaddrFromNet(raddr)
	conn, err := n.dialer.DialContext(n.ctx, "tcp", destination)
	if err != nil {
		return nil, err
	}
	// the DefaultDialer should always return a net.TCPConn
	if tcpConn, ok := conn.(transport.TCPConn); ok {
		return tcpConn, nil
	}
	conn.Close()
	return nil, &net.OpError{
		Op:   "dial",
		Net:  network,
		Addr: raddr,
		Err:  net.InvalidAddrError("not a TCP connection"),
	}
}

// maybe well implement these later. might be useful
// func (n *rtcNet) ResolveIPAddr(network string, address string) (*net.IPAddr, error)   {}
// func (n *rtcNet) ResolveUDPAddr(network string, address string) (*net.UDPAddr, error) {}
// func (n *rtcNet) ResolveTCPAddr(network string, address string) (*net.TCPAddr, error) {}

func (n *rtcNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return &rtcDialer{n.ctx, n.dialer}
}

type rtcDialer struct {
	ctx    context.Context
	dialer N.Dialer
}

func (d *rtcDialer) Dial(network, address string) (net.Conn, error) {
	return d.dialer.DialContext(d.ctx, network, metadata.ParseSocksaddr(address))
}
