package unbounded

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
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

// DialTCP dials through n.dialer. In a running box that dialer wraps each conn for connection
// tracking and counting, and the wrapper is a plain net.Conn, so the socket under it serves pion's
// TCP-only methods. A dialer with no socket under its conn, such as a detour, is closed and the
// address dialed directly, bound off the tunnel.
func (n *rtcNet) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	conn, err := n.dialer.DialContext(n.ctx, network, metadata.SocksaddrFromNet(raddr))
	if err != nil {
		return nil, dialError(network, err)
	}
	if tcp, ok := conn.(transport.TCPConn); ok {
		return tcp, nil
	}
	if tcp, ok := common.Cast[*net.TCPConn](conn); ok {
		return &wrappedTCPConn{Conn: conn, tcpOnly: tcp}, nil
	}
	conn.Close()
	conn, err = n.dialDirect(network, raddr.String())
	if err != nil {
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

// DialUDP is DialTCP for UDP.
func (n *rtcNet) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.dialer.DialContext(n.ctx, network, metadata.SocksaddrFromNet(raddr))
	if err != nil {
		return nil, dialError(network, err)
	}
	if udp, ok := conn.(transport.UDPConn); ok {
		return udp, nil
	}
	if udp, ok := common.Cast[*net.UDPConn](conn); ok {
		return &wrappedUDPConn{Conn: conn, udpOnly: udp}, nil
	}
	conn.Close()
	conn, err = n.dialDirect(network, raddr.String())
	if err != nil {
		return nil, err
	}
	return conn.(*net.UDPConn), nil
}

func (n *rtcNet) dialDirect(network, address string) (net.Conn, error) {
	d := net.Dialer{Control: physnet.ListenControl(n.ctx)}
	conn, err := d.DialContext(n.ctx, network, address)
	if err != nil {
		return nil, dialError(network, err)
	}
	return conn, nil
}

// dialError is err without the address a *net.OpError carries: the addresses are the ICE servers
// Freddie sends, and errors reach logs.
func dialError(network string, err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		err = opErr.Err
	}
	return &net.OpError{Op: "dial", Net: network, Err: err}
}

// tcpOnly is the part of transport.TCPConn that net.Conn lacks, but for ReadFrom.
type tcpOnly interface {
	CloseRead() error
	CloseWrite() error
	SetLinger(sec int) error
	SetKeepAlive(keepalive bool) error
	SetKeepAlivePeriod(d time.Duration) error
	SetNoDelay(noDelay bool) error
	SetWriteBuffer(bytes int) error
	SetReadBuffer(bytes int) error
}

// wrappedTCPConn is a conn the sing-box dialer wrapped: Read, Write and Close go through the
// wrapper, so its tracking and counting still see them, and the TCP-only methods to the socket.
type wrappedTCPConn struct {
	net.Conn
	tcpOnly
}

func (c *wrappedTCPConn) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

// udpOnly is the part of transport.UDPConn that net.Conn lacks.
type udpOnly interface {
	SetReadBuffer(bytes int) error
	SetWriteBuffer(bytes int) error
	ReadFrom(p []byte) (n int, addr net.Addr, err error)
	ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error)
	ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error)
	WriteTo(p []byte, addr net.Addr) (n int, err error)
	WriteToUDP(b []byte, addr *net.UDPAddr) (int, error)
	WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error)
}

// wrappedUDPConn is wrappedTCPConn for UDP.
type wrappedUDPConn struct {
	net.Conn
	udpOnly
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

// errResolve is the error a failed lookup returns. It names no host: the hosts are the ICE servers
// Freddie sends, and errors reach logs.
var errResolve = errors.New("unbounded: resolve ICE server host")

// lookupTimeout bounds a lookup, since pion passes no context.
const lookupTimeout = 5 * time.Second

// lookup resolves host through sing-box's default DNS server, and reports whether it could ask
// sing-box at all. This process's sockets skip the VPN's tunnel, but the OS resolver they would use
// points into it, so stdnet's lookups time out while the VPN runs, and a TURN server given by name
// never gets a relay candidate. Naming the transport skips the DNS rules, and with them the fake-IP
// rule, whose addresses are only routable through the tunnel. With no sing-box DNS on ctx, or a
// fake-IP default, it reports false and the caller asks stdnet, as before.
func (n *rtcNet) lookup(host string) ([]netip.Addr, bool, error) {
	router := service.FromContext[adapter.DNSRouter](n.ctx)
	transports := service.FromContext[adapter.DNSTransportManager](n.ctx)
	if router == nil || transports == nil {
		return nil, false, nil
	}
	def := transports.Default()
	if def == nil || def.Type() == C.DNSTypeFakeIP {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(n.ctx, lookupTimeout)
	defer cancel()
	addrs, err := router.Lookup(ctx, host, adapter.DNSQueryOptions{Transport: def})
	if err != nil {
		return nil, true, errResolve
	}
	return addrs, true, nil
}

// pick is the first of addrs in network's family ("4" or "6" suffix), or the first at all.
func pick(network string, addrs []netip.Addr) (netip.Addr, error) {
	for _, a := range addrs {
		a = a.Unmap()
		switch {
		case strings.HasSuffix(network, "4") && !a.Is4():
		case strings.HasSuffix(network, "6") && !a.Is6():
		default:
			return a, nil
		}
	}
	return netip.Addr{}, errResolve
}

// resolve is the address and port for a "host:port" pion gives, through lookup, or ok=false when
// the host is a literal or sing-box can't be asked.
func (n *rtcNet) resolve(network, address string) (netip.Addr, int, bool, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return netip.Addr{}, 0, false, nil
	}
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return netip.Addr{}, 0, false, nil
	}
	addrs, ok, err := n.lookup(host)
	if !ok || err != nil {
		return netip.Addr{}, 0, ok, err
	}
	a, err := pick(network, addrs)
	if err != nil {
		return netip.Addr{}, 0, true, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		port, err = net.LookupPort(network, portStr)
		if err != nil {
			return netip.Addr{}, 0, true, errResolve
		}
	}
	return a, port, true, nil
}

func (n *rtcNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	a, port, ok, err := n.resolve(network, address)
	if !ok {
		return n.Net.ResolveUDPAddr(network, address)
	}
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: a.AsSlice(), Port: port, Zone: a.Zone()}, nil
}

func (n *rtcNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	a, port, ok, err := n.resolve(network, address)
	if !ok {
		return n.Net.ResolveTCPAddr(network, address)
	}
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: a.AsSlice(), Port: port, Zone: a.Zone()}, nil
}

func (n *rtcNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	if _, err := netip.ParseAddr(address); err == nil || address == "" {
		return n.Net.ResolveIPAddr(network, address)
	}
	addrs, ok, err := n.lookup(address)
	if !ok {
		return n.Net.ResolveIPAddr(network, address)
	}
	if err != nil {
		return nil, err
	}
	a, err := pick(network, addrs)
	if err != nil {
		return nil, err
	}
	return &net.IPAddr{IP: a.AsSlice(), Zone: a.Zone()}, nil
}

// NewProtectedNet is a pion transport.Net for WebRTC run in the process that serves ctx's sing-box
// VPN, such as the app's Unbounded widget: it dials through sing-box's default dialer, binds its
// sockets to the physical interface, gathers no candidate on the VPN's own interface, and resolves
// through sing-box's DNS. With a ctx carrying no sing-box services it behaves as pion's own Net.
func NewProtectedNet(ctx context.Context) (transport.Net, error) {
	d, err := dialer.New(ctx, option.DialerOptions{}, false)
	if err != nil {
		return nil, fmt.Errorf("unbounded: build sing-box dialer: %w", err)
	}
	return newRTCNet(ctx, d, nil)
}
