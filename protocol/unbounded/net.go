package unbounded

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
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
	// directFallback lets DialTCP and DialUDP dial directly when the dialer's conn has no socket.
	// It is false with any dialer option, such as a detour, which a direct dial would skip.
	directFallback bool
	// resolveHosts has Dial resolve a host before dialing, for a dialer that takes only addresses.
	// An outbound's dialer gets the host, which a detour may resolve through its proxy.
	resolveHosts bool
}

var _ transport.Net = (*rtcNet)(nil)

func newRTCNet(ctx context.Context, dialer N.Dialer, logger log.ContextLogger, directFallback bool) (*rtcNet, error) {
	snet, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &rtcNet{
		Net:            snet,
		ctx:            ctx,
		dialer:         dialer,
		logger:         logger,
		directFallback: directFallback,
	}, nil
}

// interfaceFilter is which interfaces pion must not see: the VPN's own and any other tunnel.
type interfaceFilter struct {
	// mine is the set the VPN serving the ctx registered as its own: its tunnel.
	mine map[string]bool
	// def is the VPN's default interface, kept even when it looks like a tunnel.
	def string
}

// tunnelPrefixes name tunnels that may lack the point-to-point flag, such as tap and WireGuard.
var tunnelPrefixes = []string{"utun", "tun", "tap", "ppp", "ipsec", "wg", "gif", "stf", "tailscale"}

// cellularPrefixes name mobile data links, some point-to-point, such as iOS's pdp_ip and Android's
// clat v4- interfaces: the device's way out, not another VPN.
var cellularPrefixes = []string{"pdp_ip", "rmnet", "ccmni", "wwan", "v4-"}

// isTunnel is whether an interface looks like a tunnel: point-to-point or a tunnel's name. Windows
// flags only PPP and tunnel adapters, so Wintun and TAP adapters, with names their apps choose, pass.
func isTunnel(name string, flags net.Flags) bool {
	if hasAnyPrefix(name, cellularPrefixes) {
		return false
	}
	return flags&net.FlagPointToPoint != 0 || hasAnyPrefix(name, tunnelPrefixes)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (f interfaceFilter) hides(name string, flags net.Flags) bool {
	return f.mine[name] || (name != f.def && isTunnel(name, flags))
}

// interfaceFilter is read on each call, since the tunnel comes back under a new name after a
// restart and the default interface changes with the network. With no VPN it hides only tunnels.
func (n *rtcNet) interfaceFilter() interfaceFilter {
	nm := service.FromContext[adapter.NetworkManager](n.ctx)
	if nm == nil {
		return interfaceFilter{}
	}
	monitor := nm.InterfaceMonitor()
	if monitor == nil {
		return interfaceFilter{}
	}
	var f interfaceFilter
	if names := monitor.MyInterfaces(); len(names) > 0 {
		f.mine = make(map[string]bool, len(names))
		for _, name := range names {
			f.mine[name] = true
		}
	}
	if def := monitor.DefaultInterface(); def != nil {
		f.def = def.Name
	}
	return f
}

// Interfaces lists the host's interfaces for pion's host candidates, without the VPN's own or any
// other tunnel. A candidate on the tunnel's address sends into the tunnel, even from this process
// whose other sockets skip it, and physnet would pin its socket there: its checks then reach
// whichever outbound the tunnel routes to, which for a personal proxy is the very helper they are
// trying to reach. Another app's VPN would carry them off the same way, or loop them back into ours.
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
	f := n.interfaceFilter()
	kept := make([]*transport.Interface, 0, len(ifs))
	for _, i := range ifs {
		if !f.hides(i.Name, i.Flags) {
			kept = append(kept, i)
		}
	}
	return kept, nil
}

// InterfaceByIndex is Interfaces' lookup by index: hidden interfaces aren't found.
func (n *rtcNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	i, err := fresh.InterfaceByIndex(index)
	if err != nil {
		return nil, err
	}
	if n.interfaceFilter().hides(i.Name, i.Flags) {
		return nil, transport.ErrInterfaceNotFound
	}
	return i, nil
}

// InterfaceByName is Interfaces' lookup by name: hidden interfaces aren't found.
func (n *rtcNet) InterfaceByName(name string) (*transport.Interface, error) {
	fresh, err := newStdNet()
	if err != nil {
		return nil, err
	}
	i, err := fresh.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	if n.interfaceFilter().hides(i.Name, i.Flags) {
		return nil, transport.ErrInterfaceNotFound
	}
	return i, nil
}

// Dial resolves a host first with resolveHosts, through lookup or else stdnet. Its errors name no
// address, as DialTCP's don't.
func (n *rtcNet) Dial(network, address string) (net.Conn, error) {
	dest := metadata.ParseSocksaddr(address)
	if n.resolveHosts && dest.IsFqdn() {
		a, err := n.resolveHost(network, dest.Fqdn)
		if err != nil {
			return nil, dialError(network, err)
		}
		dest = metadata.SocksaddrFrom(a, dest.Port)
	}
	conn, err := n.dialer.DialContext(n.ctx, network, dest)
	if err != nil {
		return nil, dialError(network, err)
	}
	return conn, nil
}

// resolveHost is host's address in network's family, through lookup, or stdnet when sing-box can't
// be asked.
func (n *rtcNet) resolveHost(network, host string) (netip.Addr, error) {
	addrs, ok, err := n.lookup(host)
	if err != nil {
		return netip.Addr{}, err
	}
	if ok {
		return pick(network, addrs)
	}
	ipNet := "ip"
	switch {
	case strings.HasSuffix(network, "4"):
		ipNet = "ip4"
	case strings.HasSuffix(network, "6"):
		ipNet = "ip6"
	}
	ip, err := n.Net.ResolveIPAddr(ipNet, host)
	if err != nil {
		return netip.Addr{}, errResolve
	}
	a, ok := netip.AddrFromSlice(ip.IP)
	if !ok {
		return netip.Addr{}, errResolve
	}
	return a.Unmap(), nil
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
// TCP-only methods. A conn with no socket under it, such as a detour's through a QUIC- or mux-based
// outbound or a tcp_fast_open one, is closed. With no dialer options the address is then dialed
// directly, bound off the tunnel; with any the dial fails, since a direct dial would drop them: a
// skipped detour, for one, would show the server this host's address.
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
	conn, err = n.dialDirect(network, raddr)
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
	conn, err = n.dialDirect(network, raddr)
	if err != nil {
		return nil, err
	}
	return conn.(*net.UDPConn), nil
}

// errNoSocket is the error a dial fails with when the dialer's conn has no socket and a direct dial
// isn't allowed.
var errNoSocket = errors.New("unbounded: dialer's conn has no socket for pion, and its options rule out a direct dial")

// directFallbackAllowed reports whether an outbound's rtcNet may dial directly: only with no dialer
// options at all, since the direct dial honours none of them.
func directFallbackAllowed(opts option.DialerOptions) bool {
	return reflect.DeepEqual(opts, option.DialerOptions{})
}

func (n *rtcNet) dialDirect(network string, raddr net.Addr) (net.Conn, error) {
	if !n.directFallback {
		return nil, dialError(network, errNoSocket)
	}
	d := physicalDialer(n.ctx)
	conn, err := d.DialContext(n.ctx, network, raddr.String())
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

// udpOnly is the socket controls of transport.UDPConn that net.Conn lacks.
type udpOnly interface {
	SetReadBuffer(bytes int) error
	SetWriteBuffer(bytes int) error
}

// wrappedUDPConn is wrappedTCPConn for UDP: every datagram goes through the wrapper, through its
// ReadFrom and WriteTo where it has them, and only the socket controls go to the socket.
type wrappedUDPConn struct {
	net.Conn
	udpOnly
}

// ReadFrom, without the wrapper's, is a Read: the conn is dialed, so its peer sent every datagram.
func (c *wrappedUDPConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if pc, ok := c.Conn.(net.PacketConn); ok {
		return pc.ReadFrom(p)
	}
	n, err := c.Conn.Read(p)
	return n, c.Conn.RemoteAddr(), err
}

func (c *wrappedUDPConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, addr, err := c.ReadFrom(b)
	udpAddr, _ := addr.(*net.UDPAddr)
	return n, udpAddr, err
}

// WriteTo, without the wrapper's, fails as a dialed *net.UDPConn's does.
func (c *wrappedUDPConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if pc, ok := c.Conn.(net.PacketConn); ok {
		return pc.WriteTo(p, addr)
	}
	return 0, &net.OpError{Op: "write", Net: "udp", Err: net.ErrWriteToConnected}
}

func (c *wrappedUDPConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	return c.WriteTo(b, addr)
}

// ReadMsgUDP and WriteMsgUDP carry control messages the wrapper can't, and the socket would skip
// the wrapper, so they fail. pion reads and writes a dialed conn with Read and Write.
func (c *wrappedUDPConn) ReadMsgUDP(_, _ []byte) (int, int, int, *net.UDPAddr, error) {
	return 0, 0, 0, nil, &net.OpError{Op: "read", Net: "udp", Err: errors.ErrUnsupported}
}

func (c *wrappedUDPConn) WriteMsgUDP(_, _ []byte, _ *net.UDPAddr) (int, int, error) {
	return 0, 0, &net.OpError{Op: "write", Net: "udp", Err: errors.ErrUnsupported}
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
	port, err := parsePort(network, portStr)
	if err != nil {
		return netip.Addr{}, 0, true, errResolve
	}
	return a, port, true, nil
}

// parsePort is a port number in range, or a service name's port.
func parsePort(network, s string) (int, error) {
	if s != "" && strings.ContainsAny(s[:1], "+-0123456789") {
		port, err := strconv.ParseUint(s, 10, 16)
		return int(port), err
	}
	return net.LookupPort(network, s)
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
//
// sing-box's default dialer binds only with auto_detect_interface or a default interface set, so
// in a VPN with neither it dials through a physDialer instead. The box's dialer stays wherever it
// binds, for the marks and connection tracking it adds.
func NewProtectedNet(ctx context.Context) (transport.Net, error) {
	n, err := newRTCNet(ctx, nil, nil, true)
	if err != nil {
		return nil, err
	}
	n.resolveHosts = true
	if nm := service.FromContext[adapter.NetworkManager](ctx); nm != nil &&
		!nm.AutoDetectInterface() && nm.DefaultOptions().BindInterface == "" {
		n.dialer = &physDialer{dialer: physicalDialer(ctx)}
		return n, nil
	}
	n.dialer, err = dialer.New(ctx, option.DialerOptions{}, false)
	if err != nil {
		return nil, fmt.Errorf("unbounded: build sing-box dialer: %w", err)
	}
	return n, nil
}

// physDialer is an N.Dialer whose every socket gets physnet's control. Like sing-box's default
// dialer it takes addresses: rtcNet resolves hosts before it dials.
type physDialer struct {
	dialer net.Dialer
}

// physicalDialer is a net.Dialer bound off the tunnel, with sing-box's connect timeout.
func physicalDialer(ctx context.Context) net.Dialer {
	return net.Dialer{Timeout: C.TCPConnectTimeout, Control: physnet.ListenControl(ctx)}
}

func (d *physDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	return d.dialer.DialContext(ctx, network, destination.String())
}

// ListenPacket picks the family as sing-box's default dialer does.
func (d *physDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	network := N.NetworkUDP
	if destination.IsIPv4() && !destination.Addr.IsUnspecified() {
		network += "4"
	}
	lc := net.ListenConfig{Control: d.dialer.Control}
	return lc.ListenPacket(ctx, network, "")
}
