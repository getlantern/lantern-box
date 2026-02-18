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

var _ transport.Net = (*rtcNet)(nil)

// rtcNet is a [transport.Net] implementation that uses a N.Dialer for dialing.
// It wraps the base [stdnet.Net] and overrides the Dial and ListenPacket methods
// to use the DefaultDialer.
type rtcNet struct {
	*stdnet.Net
	ctx    context.Context
	dialer N.Dialer
	logger log.ContextLogger
}

type logDialer struct {
	d      N.Dialer
	logger log.ContextLogger
}

func (d *logDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	d.logger.Info("dialing with sing-box dialer", "network", network, "destination", destination)
	return d.d.DialContext(ctx, network, destination)
}

func (d *logDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	d.logger.Info("listening with sing-box dialer", "destination", destination)
	return d.d.ListenPacket(ctx, destination)
}

func newRTCNet(ctx context.Context, dialer N.Dialer, logger log.ContextLogger) (*rtcNet, error) {
	snet, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &rtcNet{
		Net:    snet,
		ctx:    ctx,
		dialer: &logDialer{dialer, logger},
		logger: logger,
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
	conn, err := n.dialer.DialContext(n.ctx, network, destination)
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
	conn, err := n.dialer.DialContext(n.ctx, network, destination)
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
func (n *rtcNet) ResolveIPAddr(network string, address string) (*net.IPAddr, error) {
	n.logger.Debug("resolving IP address", "network", network, "address", address)
	return n.Net.ResolveIPAddr(network, address)
}

func (n *rtcNet) ResolveUDPAddr(network string, address string) (*net.UDPAddr, error) {
	n.logger.Debug("resolving UDP address", "network", network, "address", address)
	return n.Net.ResolveUDPAddr(network, address)
}

func (n *rtcNet) ResolveTCPAddr(network string, address string) (*net.TCPAddr, error) {
	n.logger.Debug("resolving TCP address", "network", network, "address", address)
	return n.Net.ResolveTCPAddr(network, address)
}

func (n *rtcNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return &rtcDialer{dialer, n}
}

type rtcDialer struct {
	dialer *net.Dialer
	net    *rtcNet
}

func (d *rtcDialer) Dial(network, address string) (net.Conn, error) {
	return d.dialer.Dial(network, address)
}
