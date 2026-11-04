package unbounded

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/transport/v5"
	"github.com/sagernet/sing/common/logger"

	"github.com/getlantern/lantern-box/portforward"
)

const (
	// mappedPortTimeout bounds getting a mapped port, which runs in the
	// outbound's Start and so holds up sing-box's PostStart, and also bounds
	// removing the mapping in Close.
	mappedPortTimeout = 5 * time.Second
	// mappedPortRetryAfter is how long a failed attempt is remembered. Gateway
	// discovery takes seconds on a network without one, and the bandit can
	// start several outbounds within this window.
	mappedPortRetryAfter  = 10 * time.Minute
	mappedPortDescription = "Lantern Unbounded"

	stunQueryTimeout = 1500 * time.Millisecond
	stunQueryServers = 3
)

var (
	errPortChanged = errors.New("the router maps a different external port")
	errNotPublic   = errors.New("the external address is not a public IPv4 address")
)

// portMapper is the part of portforward's Forwarder and ManualForwarder the
// mapping uses.
type portMapper interface {
	MapPort(ctx context.Context, protocol string, internalPort uint16, description string) (*portforward.Mapping, error)
	StartRenewal(ctx context.Context)
	UnmapPort(ctx context.Context) error
	ExternalIP(ctx context.Context) (string, error)
}

// mappedPort is a UDP socket whose port the router forwards to this device,
// shared by every unbounded outbound in the process. pion tells sessions apart
// by ICE username, so all of their WebRTC slots can share one mux.
type mappedPort struct {
	mux        ice.UDPMux
	externalIP string
	method     string
	port       uint16
	mapper     portMapper
	// refs is guarded by the pool's mu.
	refs int
}

// mappedPortRequest is what an outbound brings to an acquire. Only the first
// acquirer's request is used, since every later one shares its mapping.
type mappedPortRequest struct {
	// manualPort, if set, is a port the user forwarded by hand, used instead of
	// asking the gateway.
	manualPort uint16
	// stunServers find the external address when the forwarder can't say, as
	// with a manual port.
	stunServers []string
	// net enumerates local addresses for the mux's wildcard socket.
	net    transport.Net
	logger logger.ContextLogger
}

// mappedPortPool hands out one process-wide mappedPort, reference counted. Its
// func fields are the network and the gateway, so tests can replace them.
type mappedPortPool struct {
	listen     func(ctx context.Context, port uint16) (net.PacketConn, error)
	gateway    func(ctx context.Context) (portMapper, error)
	manual     func(port uint16) portMapper
	stunIP     func(ctx context.Context, conn net.PacketConn, servers []string) (string, error)
	now        func() time.Time
	timeout    time.Duration
	retryAfter time.Duration

	mu      sync.Mutex
	current *mappedPort
	// pending is closed when the attempt in flight ends, so concurrent
	// acquirers wait for it rather than start their own.
	pending  chan struct{}
	failedAt time.Time
}

var sharedMappedPorts = newMappedPortPool()

func newMappedPortPool() *mappedPortPool {
	return &mappedPortPool{
		listen: listenMappedUDP,
		gateway: func(ctx context.Context) (portMapper, error) {
			f, err := portforward.NewForwarder(ctx)
			if err != nil {
				return nil, err
			}
			// While the tunnel is up, an unbound dial reports the TUN's address,
			// and the gateway would forward the port to it.
			if ctl := bindEgressToPhysicalInterface(ctx); ctl != nil {
				f.SetLocalIPControl(ctl)
			}
			return f, nil
		},
		manual: func(port uint16) portMapper {
			return portforward.NewManualForwarder(port)
		},
		stunIP:     stunExternalIP,
		now:        time.Now,
		timeout:    mappedPortTimeout,
		retryAfter: mappedPortRetryAfter,
	}
}

// acquire returns the shared mapping, creating it if no outbound holds one, or
// nil if there is none to be had. Every non-nil result must be released.
func (p *mappedPortPool) acquire(parent context.Context, req mappedPortRequest) *mappedPort {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	for {
		p.mu.Lock()
		if p.current != nil {
			p.current.refs++
			m := p.current
			p.mu.Unlock()
			return m
		}
		if !p.failedAt.IsZero() && p.now().Sub(p.failedAt) < p.retryAfter {
			p.mu.Unlock()
			return nil
		}
		if wait := p.pending; wait != nil {
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		done := make(chan struct{})
		p.pending = done
		p.mu.Unlock()

		m, stage, err := p.establish(ctx, req)

		p.mu.Lock()
		p.pending = nil
		close(done)
		if err != nil {
			// A canceled parent is the outbound shutting down, which says
			// nothing about the network.
			if parent.Err() == nil {
				p.failedAt = p.now()
			}
			p.mu.Unlock()
			if req.logger != nil {
				req.logger.InfoContext(parent, "unbounded: no mapped UDP port, helpers reach this consumer through STUN only",
					"stage", stage,
					"reason", mappedPortFailureReason(err),
					"manual_port", req.manualPort != 0,
					"retry_in", p.retryAfter)
			}
			return nil
		}
		m.refs = 1
		p.current = m
		p.failedAt = time.Time{}
		p.mu.Unlock()
		if req.logger != nil {
			req.logger.InfoContext(parent, "unbounded: mapped UDP port ready",
				"method", m.method,
				"port", m.port)
		}
		return m
	}
}

// establish binds the socket, maps it and finds the address it is reachable at.
// On failure it releases whatever it got, and returns the stage that failed.
// Its error never carries the external address.
func (p *mappedPortPool) establish(ctx context.Context, req mappedPortRequest) (_ *mappedPort, stage string, err error) {
	conn, err := p.listen(ctx, req.manualPort)
	if err != nil {
		return nil, "bind", err
	}
	var mapper portMapper
	defer func() {
		if err == nil {
			return
		}
		_ = conn.Close()
		if mapper != nil {
			// In the background: the attempt's time is spent, and a gateway that
			// answered the add will most likely answer this.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
				defer cancel()
				_ = mapper.UnmapPort(ctx)
			}()
		}
	}()

	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, "bind", fmt.Errorf("unexpected local address type %T", conn.LocalAddr())
	}
	port := uint16(udpAddr.Port)

	var m portMapper
	if req.manualPort != 0 {
		m = p.manual(req.manualPort)
	} else {
		m, err = p.gateway(ctx)
		if err != nil {
			return nil, "discover", err
		}
	}
	mapping, err := m.MapPort(ctx, portforward.UDP, port, mappedPortDescription)
	if errors.Is(err, portforward.ErrNoLocalIP) {
		return nil, "local_ip", err
	}
	if err != nil {
		return nil, "map", err
	}
	mapper = m
	// The external address is advertised on the mux's own port.
	if mapping.ExternalPort != port || mapping.InternalPort != port {
		return nil, "map", errPortChanged
	}

	ip, err := mapper.ExternalIP(ctx)
	if err != nil {
		return nil, "external_ip", err
	}
	if ip == "" {
		ip, err = p.stunIP(ctx, conn, req.stunServers)
		if err != nil {
			return nil, "stun", err
		}
	}
	// Behind a second NAT, or a CGNAT, the router's WAN address is not where
	// helpers can reach us.
	if !isPublicIPv4(ip) {
		return nil, "external_ip", errNotPublic
	}
	// Not the attempt's ctx, which ends with it: renewal runs until UnmapPort.
	mapper.StartRenewal(context.Background())

	return &mappedPort{
		mux:        ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn, Net: req.net}),
		externalIP: ip,
		method:     mapping.Method,
		port:       port,
		mapper:     mapper,
	}, "", nil
}

// release drops one reference to m. The last one closes the mux, and with it
// the socket, and removes the mapping. Call it only once the broflake widget
// using m has stopped.
func (p *mappedPortPool) release(m *mappedPort) {
	p.mu.Lock()
	m.refs--
	if m.refs > 0 {
		p.mu.Unlock()
		return
	}
	if p.current == m {
		p.current = nil
	}
	// Under mu, so an acquirer that comes next finds a manual port free.
	_ = m.mux.Close()
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	_ = m.mapper.UnmapPort(ctx)
}

// listenMappedUDP binds the mapped socket on port, or a random one if port is
// 0, kept off the VPN tunnel the way rtcNet.ListenUDP keeps pion's sockets off
// it. Wildcard, since the mapping names whichever address portforward finds.
func listenMappedUDP(ctx context.Context, port uint16) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: bindEgressToPhysicalInterface(ctx)}
	return lc.ListenPacket(ctx, "udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(port))))
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublicIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.Is4() &&
		a.IsGlobalUnicast() &&
		!a.IsPrivate() &&
		!cgnat.Contains(a) &&
		a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// stunExternalIP asks STUN servers, from conn itself, for the address conn's
// traffic leaves the router from. A manually forwarded port comes with no
// gateway to ask.
func stunExternalIP(ctx context.Context, conn net.PacketConn, servers []string) (string, error) {
	batch, err := randomSTUNBatch(servers)(stunQueryServers)
	if err != nil {
		return "", err
	}
	if len(batch) == 0 {
		return "", errors.New("no STUN servers")
	}
	// The mux reads conn next and expects no deadline.
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	var errs []error
	for _, server := range batch {
		ip, err := stunQuery(ctx, conn, server)
		if err == nil {
			return ip, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return "", errors.Join(errs...)
}

func stunQuery(ctx context.Context, conn net.PacketConn, server string) (string, error) {
	uri, err := stun.ParseURI(server)
	if err != nil {
		return "", err
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", uri.Host)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New("STUN server has no IPv4 address")
	}
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(stunQueryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	dst := net.UDPAddrFromAddrPort(netip.AddrPortFrom(addrs[0].Unmap(), uint16(uri.Port)))
	if _, err := conn.WriteTo(req.Raw, dst); err != nil {
		return "", err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return "", err
		}
		// The port is forwarded, so anything on the internet can send to it.
		res := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if res.Decode() != nil || res.TransactionID != req.TransactionID {
			continue
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(res); err != nil {
			return "", err
		}
		return xor.IP.String(), nil
	}
}

// mappedPortFailureReason sorts a failure for the log without its text, which
// can carry addresses.
func mappedPortFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, portforward.ErrNoPortForwarding):
		return "no_gateway"
	case errors.Is(err, errPortChanged):
		return "port_changed"
	case errors.Is(err, errNotPublic):
		return "not_public"
	}
	return "error"
}
