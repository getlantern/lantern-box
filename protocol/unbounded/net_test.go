package unbounded

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/transport/v5"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMonitor reports names as the VPN's own interfaces. Every other method panics, unused.
type fakeMonitor struct {
	tun.DefaultInterfaceMonitor
	names []string
}

func (m *fakeMonitor) MyInterfaces() []string { return m.names }

// fakeNetworkManager gives rtcNet an interface monitor and nothing else.
type fakeNetworkManager struct {
	adapter.NetworkManager
	monitor tun.DefaultInterfaceMonitor
}

func (m *fakeNetworkManager) InterfaceMonitor() tun.DefaultInterfaceMonitor { return m.monitor }

// hostInterface is a real interface of this machine with an address, which the test pretends is
// the tunnel.
func hostInterface(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	require.NoError(t, err)
	for _, i := range ifs {
		if addrs, _ := i.Addrs(); len(addrs) > 0 {
			return i.Name
		}
	}
	t.Skip("no interface with an address")
	return ""
}

func names(ifs []*transport.Interface) []string {
	var out []string
	for _, i := range ifs {
		out = append(out, i.Name)
	}
	return out
}

func TestRTCNetHidesTheVPNsInterfaces(t *testing.T) {
	mine := hostInterface(t)
	ctx := service.ContextWith[adapter.NetworkManager](context.Background(),
		&fakeNetworkManager{monitor: &fakeMonitor{names: []string{mine}}})
	n, err := newRTCNet(ctx, nil, nil)
	require.NoError(t, err)

	ifs, err := n.Interfaces()
	require.NoError(t, err)
	assert.NotContains(t, names(ifs), mine)

	_, err = n.InterfaceByName(mine)
	assert.True(t, errors.Is(err, transport.ErrInterfaceNotFound), "got %v", err)

	host, err := net.InterfaceByName(mine)
	require.NoError(t, err)
	_, err = n.InterfaceByIndex(host.Index)
	assert.True(t, errors.Is(err, transport.ErrInterfaceNotFound), "got %v", err)
}

func TestRTCNetHidesNothingWithoutAMonitor(t *testing.T) {
	mine := hostInterface(t)
	for name, ctx := range map[string]context.Context{
		"no network manager": context.Background(),
		"no monitor": service.ContextWith[adapter.NetworkManager](context.Background(),
			&fakeNetworkManager{}),
	} {
		t.Run(name, func(t *testing.T) {
			n, err := newRTCNet(ctx, nil, nil)
			require.NoError(t, err)
			ifs, err := n.Interfaces()
			require.NoError(t, err)
			assert.Contains(t, names(ifs), mine)
			_, err = n.InterfaceByName(mine)
			assert.NoError(t, err)
		})
	}
}

type fakeTransport struct {
	adapter.DNSTransport
	typ string
}

func (t *fakeTransport) Type() string { return t.typ }

type fakeTransports struct {
	adapter.DNSTransportManager
	def adapter.DNSTransport
}

func (m *fakeTransports) Default() adapter.DNSTransport { return m.def }

// fakeRouter answers every lookup with addrs, or err, and records the transport it was asked to use.
type fakeRouter struct {
	adapter.DNSRouter
	addrs []netip.Addr
	err   error
	used  adapter.DNSTransport
	asked []string
}

func (r *fakeRouter) Lookup(_ context.Context, domain string, o adapter.DNSQueryOptions) ([]netip.Addr, error) {
	r.used = o.Transport
	r.asked = append(r.asked, domain)
	return r.addrs, r.err
}

func dnsCtx(router *fakeRouter, typ string) context.Context {
	ctx := service.ContextWith[adapter.DNSRouter](context.Background(), router)
	return service.ContextWith[adapter.DNSTransportManager](ctx, &fakeTransports{def: &fakeTransport{typ: typ}})
}

func TestRTCNetResolvesThroughSingBox(t *testing.T) {
	router := &fakeRouter{addrs: []netip.Addr{
		netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("192.0.2.7"),
	}}
	n, err := newRTCNet(dnsCtx(router, C.DNSTypeHTTPS), nil, nil)
	require.NoError(t, err)

	udp, err := n.ResolveUDPAddr("udp4", "turn.example:3478")
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.7:3478", udp.String())
	assert.Equal(t, C.DNSTypeHTTPS, router.used.Type(), "the default transport, set explicitly")

	tcp, err := n.ResolveTCPAddr("tcp6", "turn.example:443")
	require.NoError(t, err)
	assert.Equal(t, "[2001:db8::1]:443", tcp.String())

	ip, err := n.ResolveIPAddr("ip", "turn.example")
	require.NoError(t, err)
	assert.Equal(t, "2001:db8::1", ip.String(), "with no family asked, the first answer")
}

func TestRTCNetPassesLiteralsThrough(t *testing.T) {
	router := &fakeRouter{}
	n, err := newRTCNet(dnsCtx(router, C.DNSTypeHTTPS), nil, nil)
	require.NoError(t, err)
	udp, err := n.ResolveUDPAddr("udp4", "192.0.2.9:3478")
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.9:3478", udp.String())
	assert.Empty(t, router.asked)
}

func TestRTCNetFallsBackWithoutSingBoxDNS(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no router":       context.Background(),
		"fake-IP default": dnsCtx(&fakeRouter{err: errors.New("must not be asked")}, C.DNSTypeFakeIP),
	} {
		t.Run(name, func(t *testing.T) {
			n, err := newRTCNet(ctx, nil, nil)
			require.NoError(t, err)
			udp, err := n.ResolveUDPAddr("udp4", "localhost:3478")
			require.NoError(t, err)
			assert.True(t, udp.IP.IsLoopback())
		})
	}
}

func TestRTCNetResolveErrorsNameNoHost(t *testing.T) {
	for name, router := range map[string]*fakeRouter{
		"lookup fails": {err: errors.New("lookup secret.example: boom")},
		"wrong family": {addrs: []netip.Addr{netip.MustParseAddr("2001:db8::1")}},
	} {
		t.Run(name, func(t *testing.T) {
			n, err := newRTCNet(dnsCtx(router, C.DNSTypeHTTPS), nil, nil)
			require.NoError(t, err)
			_, err = n.ResolveUDPAddr("udp4", "secret.example:3478")
			require.Error(t, err)
			assert.True(t, errors.Is(err, errResolve))
			assert.False(t, strings.Contains(err.Error(), "secret.example"), "error %q names the host", err)
		})
	}
}

func TestNewProtectedNetWithoutAVPN(t *testing.T) {
	n, err := NewProtectedNet(context.Background())
	require.NoError(t, err)
	ifs, err := n.Interfaces()
	require.NoError(t, err)
	assert.NotEmpty(t, ifs)
}

// trackingCtx is the ctx a running box gives its dialers: with a connection manager, whose
// DefaultDialer wraps every conn it dials.
func trackingCtx() (context.Context, *route.ConnectionManager) {
	cm := route.NewConnectionManager(nil)
	return service.ContextWith[adapter.ConnectionManager](context.Background(), cm), cm
}

func TestRTCNetDialsThroughATrackingDialer(t *testing.T) {
	ctx, cm := trackingCtx()
	n, err := NewProtectedNet(ctx)
	require.NoError(t, err)

	t.Run("tcp", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			c, _ := ln.Accept()
			accepted <- c
		}()

		c, err := n.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
		require.NoError(t, err)
		peer := <-accepted
		require.NotNil(t, peer)
		defer peer.Close()
		assert.Equal(t, 1, cm.Count(), "the dialer's tracking still holds the conn")
		require.NoError(t, c.SetNoDelay(true))
		_, err = c.Write([]byte("ping"))
		require.NoError(t, err)
		buf := make([]byte, 4)
		_, err = io.ReadFull(peer, buf)
		require.NoError(t, err)
		assert.Equal(t, "ping", string(buf))
		require.NoError(t, c.Close())
		assert.Equal(t, 0, cm.Count(), "Close went through the tracking")
	})

	t.Run("udp", func(t *testing.T) {
		peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(t, err)
		defer peer.Close()

		c, err := n.DialUDP("udp4", nil, peer.LocalAddr().(*net.UDPAddr))
		require.NoError(t, err)
		assert.Equal(t, 1, cm.Count(), "the dialer's tracking still holds the conn")
		require.NoError(t, c.SetReadBuffer(1<<16))
		_, err = c.Write([]byte("ping"))
		require.NoError(t, err)
		buf := make([]byte, 4)
		require.NoError(t, peer.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, _, err = peer.ReadFromUDP(buf)
		require.NoError(t, err)
		assert.Equal(t, "ping", string(buf))
		require.NoError(t, c.Close())
		assert.Equal(t, 0, cm.Count(), "Close went through the tracking")
	})
}

// pipeDialer dials a conn with no socket under it, as a detour through another outbound would.
type pipeDialer struct {
	N.Dialer
	closed chan struct{}
}

func (d *pipeDialer) DialContext(context.Context, string, metadata.Socksaddr) (net.Conn, error) {
	c, other := net.Pipe()
	go func() {
		_, _ = io.Copy(io.Discard, other)
		close(d.closed)
	}()
	return c, nil
}

func TestRTCNetDialsDirectlyWhenTheDialerHidesTheSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	d := &pipeDialer{closed: make(chan struct{})}
	n, err := newRTCNet(context.Background(), d, nil)
	require.NoError(t, err)
	c, err := n.DialTCP("tcp4", nil, ln.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer c.Close()
	assert.Equal(t, ln.Addr().String(), c.RemoteAddr().String())
	select {
	case <-d.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the dialer's conn was left open")
	}
}

func TestRTCNetDialErrorsNameNoAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().(*net.TCPAddr)
	require.NoError(t, ln.Close())
	n, err := NewProtectedNet(context.Background())
	require.NoError(t, err)
	_, err = n.DialTCP("tcp4", nil, addr)
	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), strconv.Itoa(addr.Port)), "error %q names the address", err)
}
