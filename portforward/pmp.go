package portforward

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackpal/gateway"
	"github.com/sagernet/sing/common/control"
)

// NAT-PMP (RFC 6886) and its successor PCP (RFC 6887) are what Apple routers
// and many ISP gateways speak instead of UPnP. Both are UDP request/response
// protocols on the gateway's port 5351. The clients here satisfy igdClient, so
// a Forwarder's timeouts, renewal and teardown work the same for them as for
// UPnP.
//
// Neither protocol is told which external port to use, only asked. A gateway
// that assigns a different port has its mapping deleted and reported as a
// refusal: callers advertise the internal port, so the two must match.

const (
	pmpServerPort = 5351
	// pmpTimeout bounds one request, resends included. A gateway on the LAN
	// answers in milliseconds, and a Forwarder's own timeouts are far longer.
	pmpTimeout = 2 * time.Second
	// pmpProbeTimeout bounds discovery, which runs before UPnP's and so delays
	// it on networks without either protocol.
	pmpProbeTimeout = time.Second
	// pmpFirstResend is the RFCs' initial retransmission interval, doubled on
	// each resend.
	pmpFirstResend = 250 * time.Millisecond
	// minGrantedLease is the shortest lease, in seconds, a new mapping may be
	// granted. Anything shorter would need renewing every few seconds. PCP
	// gateways should grant at least 120 (RFC 6887 §15).
	minGrantedLease = 10
)

var (
	// errPMPVersion is returned when the gateway answers in the other
	// protocol's version, so discovery can move on at once rather than wait out
	// the timeout.
	errPMPVersion = errors.New("gateway does not speak this protocol version")
	// errOutcomeUnknown is returned when a request's outcome is unknown: the
	// gateway never answered, or the transport failed after sending. Unlike a
	// refusal, the request may still have taken effect.
	errOutcomeUnknown = errors.New("outcome unknown")
)

// pmpExchange sends the request build returns to gw, resending on the RFCs'
// schedule, and returns the first response accept takes, or accept's error.
// build gets the socket's local address, which PCP puts in its requests. Once
// a request has gone out, a transport error leaves its outcome unknown, so it
// is wrapped in errOutcomeUnknown.
func pmpExchange(gw *net.UDPAddr, timeout time.Duration, fn control.Func, build func(local net.IP) []byte, accept func(b []byte) (bool, error)) ([]byte, error) {
	d := net.Dialer{Control: fn}
	c, err := d.Dial("udp4", gw.String())
	if err != nil {
		return nil, err
	}
	conn := c.(*net.UDPConn)
	defer conn.Close()
	req := build(conn.LocalAddr().(*net.UDPAddr).IP)
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1100)
	sent := false
	uncertain := func(err error) error {
		if sent {
			return fmt.Errorf("%w: %w", errOutcomeUnknown, err)
		}
		return err
	}
	for wait := pmpFirstResend; ; wait *= 2 {
		if _, err := conn.Write(req); err != nil {
			return nil, uncertain(err)
		}
		sent = true
		next := time.Now().Add(wait)
		if next.After(deadline) {
			next = deadline
		}
		if err := conn.SetReadDeadline(next); err != nil {
			return nil, uncertain(err)
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					// Typically ICMP port unreachable: nothing listens on 5351.
					return nil, uncertain(err)
				}
				break
			}
			ok, err := accept(buf[:n])
			if err != nil {
				return nil, err
			}
			if ok {
				return append([]byte(nil), buf[:n]...), nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: no answer from %s within %s", errOutcomeUnknown, gw, timeout)
		}
	}
}

// mapFunc sends one mapping request and returns the external port and the
// lifetime, in seconds, the gateway granted. A lifetime of 0 deletes the
// mapping for internalPort.
type mapFunc func(protocol string, internalPort, externalPort uint16, lifetime uint32) (port uint16, granted uint32, err error)

// pmpMappings is what a NAT-PMP or PCP client knows of its live mappings: the
// lifetime the gateway granted each, which can be shorter than requested.
type pmpMappings struct {
	mu      sync.Mutex
	granted map[pcpKey]uint32
	// quiet, when set, is the owning Forwarder's quiet-logs switch.
	quiet *atomic.Bool
}

// setQuiet shares the owning Forwarder's quiet-logs switch. Called once, before
// any mapping is added.
func (s *pmpMappings) setQuiet(q *atomic.Bool) { s.quiet = q }

type pcpKey struct {
	protocol     string
	internalPort uint16
}

// add maps internalPort to externalPort, or renews that mapping.
func (s *pmpMappings) add(method string, mapPort mapFunc, protocol string, internalPort, externalPort uint16, lifetime uint32) error {
	k := pcpKey{protocol, internalPort}
	s.mu.Lock()
	_, renewing := s.granted[k]
	s.mu.Unlock()
	got, granted, err := mapPort(protocol, internalPort, externalPort, lifetime)
	if err != nil {
		// The gateway may have created a new mapping and lost only its answer,
		// and the Forwarder won't track a mapping whose add failed. A renewal's
		// mapping is already tracked, and teardown removes it.
		if !renewing && errors.Is(err, errOutcomeUnknown) {
			if _, _, err := mapPort(protocol, internalPort, externalPort, 0); err != nil {
				logWarn(s.quiet != nil && s.quiet.Load(), "portforward: could not remove a mapping whose answer was lost", "method", method, "err", err)
			}
		}
		return err
	}
	if got != externalPort {
		if _, _, err := mapPort(protocol, internalPort, got, 0); err != nil {
			logWarn(s.quiet != nil && s.quiet.Load(), "portforward: could not remove a mapping on the wrong port", "method", method, "err", err)
		}
		s.forget(k)
		return fmt.Errorf("%s: gateway assigned external port %d, not %d", method, got, externalPort)
	}
	if !renewing && granted < minGrantedLease {
		if _, _, err := mapPort(protocol, internalPort, externalPort, 0); err != nil {
			logWarn(s.quiet != nil && s.quiet.Load(), "portforward: could not remove a mapping with too short a lease", "method", method, "err", err)
		}
		return fmt.Errorf("%s: gateway granted a lease of only %ds", method, granted)
	}
	s.mu.Lock()
	s.granted[k] = granted
	s.mu.Unlock()
	return nil
}

// remove deletes the mapping for internalPort. AddPortMapping only keeps
// mappings whose external port equals their internal one, so callers pass
// either.
func (s *pmpMappings) remove(mapPort mapFunc, protocol string, internalPort uint16) error {
	if _, _, err := mapPort(protocol, internalPort, internalPort, 0); err != nil {
		return err
	}
	s.forget(pcpKey{protocol, internalPort})
	return nil
}

func (s *pmpMappings) forget(k pcpKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.granted, k)
}

// lease is the lifetime the gateway granted the mapping for internalPort.
func (s *pmpMappings) lease(protocol string, internalPort uint16) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.granted[pcpKey{protocol, internalPort}]
	return time.Duration(g) * time.Second, ok
}

// natPMPClient speaks NAT-PMP. The gateway maps to whichever host sent the
// request, so internalClient is unused.
type natPMPClient struct {
	gw      *net.UDPAddr
	timeout time.Duration
	// control, if set, controls the socket of every request. See BoundTo.
	control control.Func
	pmpMappings
}

func newNATPMPClient(gw *net.UDPAddr, timeout time.Duration) *natPMPClient {
	return &natPMPClient{gw: gw, timeout: timeout, pmpMappings: pmpMappings{granted: map[pcpKey]uint32{}}}
}

const (
	natPMPVersion      = 0
	natPMPOpExternalIP = 0
	natPMPOpMapUDP     = 1
	natPMPOpMapTCP     = 2
)

func (c *natPMPClient) request(op byte, req []byte, minLen int) ([]byte, error) {
	res, err := pmpExchange(c.gw, c.timeout, c.control, func(net.IP) []byte { return req },
		func(b []byte) (bool, error) {
			if len(b) >= 2 && b[0] != natPMPVersion {
				return false, errPMPVersion
			}
			if len(b) < 4 || b[1] != 128+op {
				return false, nil
			}
			if code := binary.BigEndian.Uint16(b[2:4]); code != 0 {
				return false, fmt.Errorf("NAT-PMP result code %d", code)
			}
			return len(b) >= minLen, nil
		})
	if err != nil {
		return nil, fmt.Errorf("nat-pmp: %w", err)
	}
	return res, nil
}

// mapPort is NAT-PMP's mapFunc.
func (c *natPMPClient) mapPort(protocol string, internalPort, externalPort uint16, lifetime uint32) (uint16, uint32, error) {
	op := byte(natPMPOpMapTCP)
	if protocol == UDP {
		op = natPMPOpMapUDP
	}
	if lifetime == 0 {
		// RFC 6886 §3.4: a deletion's suggested external port must be zero.
		externalPort = 0
	}
	req := make([]byte, 12)
	req[0], req[1] = natPMPVersion, op
	binary.BigEndian.PutUint16(req[4:6], internalPort)
	binary.BigEndian.PutUint16(req[6:8], externalPort)
	binary.BigEndian.PutUint32(req[8:12], lifetime)
	res, err := c.request(op, req, 16)
	if err != nil {
		return 0, 0, err
	}
	if got := binary.BigEndian.Uint16(res[8:10]); got != internalPort {
		return 0, 0, fmt.Errorf("nat-pmp: answer is for internal port %d, not %d", got, internalPort)
	}
	return binary.BigEndian.Uint16(res[10:12]), binary.BigEndian.Uint32(res[12:16]), nil
}

func (c *natPMPClient) AddPortMapping(_ string, externalPort uint16, protocol string, internalPort uint16, _ string, _ bool, _ string, leaseDuration uint32) error {
	return c.add("nat-pmp", c.mapPort, protocol, internalPort, externalPort, leaseDuration)
}

// DeletePortMapping deletes by internal port, which NAT-PMP keys mappings on.
func (c *natPMPClient) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	return c.remove(c.mapPort, protocol, externalPort)
}

func (c *natPMPClient) GetExternalIPAddress() (string, error) {
	res, err := c.request(natPMPOpExternalIP, []byte{natPMPVersion, natPMPOpExternalIP}, 12)
	if err != nil {
		return "", err
	}
	return net.IP(res[8:12]).String(), nil
}

// pcpClient speaks PCP. Refreshing or deleting a mapping needs the nonce that
// created it, so the client keeps one per protocol and internal port for as
// long as it lives: a renewal racing a delete may re-create the mapping, and
// only the same nonce can then remove it. PCP has no request for the external
// address alone, so the client remembers the one its last mapping reported.
type pcpClient struct {
	gw      *net.UDPAddr
	timeout time.Duration
	// control, if set, controls the socket of every request. See BoundTo.
	control control.Func
	pmpMappings

	nonces     map[pcpKey][12]byte
	externalIP net.IP
}

const (
	pcpVersion     = 2
	pcpOpAnnounce  = 0
	pcpOpMap       = 1
	pcpHeaderLen   = 24
	pcpMapLen      = 36
	pcpProtocolTCP = 6
	pcpProtocolUDP = 17
)

func newPCPClient(gw *net.UDPAddr, timeout time.Duration) *pcpClient {
	return &pcpClient{gw: gw, timeout: timeout, pmpMappings: pmpMappings{granted: map[pcpKey]uint32{}}, nonces: map[pcpKey][12]byte{}}
}

// pcpHeader builds a request header from local, the address the request is
// sent from, which a PCP server checks against the packet's source.
func pcpHeader(op byte, lifetime uint32, local net.IP) []byte {
	h := make([]byte, pcpHeaderLen)
	h[0], h[1] = pcpVersion, op
	binary.BigEndian.PutUint32(h[4:8], lifetime)
	copy(h[8:24], local.To16())
	return h
}

// pcpAccept takes a response to op that check accepts. An error response can
// carry only the common header (RFC 6887 §7.2), and then nothing in it ties it
// to the request; each request has its own socket, connected to the gateway,
// so it is taken as this request's. An error that carries a payload must pass
// check like a success.
func pcpAccept(op byte, check func(b []byte) bool) func(b []byte) (bool, error) {
	return func(b []byte) (bool, error) {
		if len(b) >= 1 && b[0] != pcpVersion {
			return false, errPMPVersion
		}
		if len(b) < pcpHeaderLen || b[1] != 0x80|op {
			return false, nil
		}
		if len(b) > pcpHeaderLen && !check(b) {
			return false, nil
		}
		if b[3] != 0 {
			return false, fmt.Errorf("PCP result code %d", b[3])
		}
		return check(b), nil
	}
}

func (c *pcpClient) nonce(protocol string, internalPort uint16) ([12]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := pcpKey{protocol, internalPort}
	if n, ok := c.nonces[k]; ok {
		return n, nil
	}
	var n [12]byte
	if _, err := rand.Read(n[:]); err != nil {
		return n, err
	}
	c.nonces[k] = n
	return n, nil
}

// mapPort is PCP's mapFunc, a MAP request.
func (c *pcpClient) mapPort(protocol string, internalPort, externalPort uint16, lifetime uint32) (uint16, uint32, error) {
	nonce, err := c.nonce(protocol, internalPort)
	if err != nil {
		return 0, 0, err
	}
	proto := byte(pcpProtocolTCP)
	if protocol == UDP {
		proto = pcpProtocolUDP
	}
	build := func(local net.IP) []byte {
		req := append(pcpHeader(pcpOpMap, lifetime, local), make([]byte, pcpMapLen)...)
		m := req[pcpHeaderLen:]
		copy(m[0:12], nonce[:])
		m[12] = proto
		binary.BigEndian.PutUint16(m[16:18], internalPort)
		binary.BigEndian.PutUint16(m[18:20], externalPort)
		// No preferred external address: the IPv4-mapped unspecified address.
		copy(m[20:36], net.IPv4zero.To16())
		return req
	}
	res, err := pmpExchange(c.gw, c.timeout, c.control, build, pcpAccept(pcpOpMap, func(b []byte) bool {
		if len(b) < pcpHeaderLen+pcpMapLen {
			return false
		}
		m := b[pcpHeaderLen:]
		return [12]byte(m[0:12]) == nonce && m[12] == proto && binary.BigEndian.Uint16(m[16:18]) == internalPort
	}))
	if err != nil {
		return 0, 0, fmt.Errorf("pcp: %w", err)
	}
	m := res[pcpHeaderLen:]
	if lifetime > 0 {
		c.mu.Lock()
		c.externalIP = net.IP(append([]byte(nil), m[20:36]...))
		c.mu.Unlock()
	}
	return binary.BigEndian.Uint16(m[18:20]), binary.BigEndian.Uint32(res[4:8]), nil
}

func (c *pcpClient) AddPortMapping(_ string, externalPort uint16, protocol string, internalPort uint16, _ string, _ bool, _ string, leaseDuration uint32) error {
	return c.add("pcp", c.mapPort, protocol, internalPort, externalPort, leaseDuration)
}

// DeletePortMapping deletes by internal port, as the mapping's nonce is keyed.
func (c *pcpClient) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	return c.remove(c.mapPort, protocol, externalPort)
}

func (c *pcpClient) GetExternalIPAddress() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.externalIP == nil {
		return "", errors.New("pcp: no mapping has reported the external address yet")
	}
	return c.externalIP.String(), nil
}

// announce asks the gateway whether it speaks PCP.
func (c *pcpClient) announce() error {
	_, err := pmpExchange(c.gw, c.timeout, c.control, func(local net.IP) []byte { return pcpHeader(pcpOpAnnounce, 0, local) },
		pcpAccept(pcpOpAnnounce, func([]byte) bool { return true }))
	if err != nil {
		return fmt.Errorf("pcp: %w", err)
	}
	return nil
}

// findGateway is the default gateway's address, or ctx's error if it ends
// first. A lookup still running then finishes on its own.
var findGateway = func(ctx context.Context) (net.IP, error) {
	type found struct {
		ip  net.IP
		err error
	}
	ch := make(chan found, 1)
	go func() {
		ip, err := gateway.DiscoverGateway()
		ch <- found{ip, err}
	}()
	select {
	case f := <-ch:
		return f.ip, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// discoverGateways lists the gateways of the routing table's routes. A var so
// tests can supply them.
var discoverGateways = gateway.DiscoverGateways

// findGatewayBound is findGateway, but with fn set it is the gateway on the
// subnet of the interface fn binds to. While a VPN tunnel holds the default
// route, the first gateway in the routing table can be the tunnel's, which
// no PCP, NAT-PMP or UPnP request should go to. With no such gateway it
// fails rather than fall back to findGateway: a bound request to a gateway
// off the local network would leave outside the tunnel.
func findGatewayBound(ctx context.Context, fn control.Func) (net.IP, error) {
	if fn == nil {
		return findGateway(ctx)
	}
	type found struct {
		ip  net.IP
		err error
	}
	ch := make(chan found, 1)
	go func() {
		ip, err := gatewayOnBoundSubnet(fn)
		ch <- found{ip, err}
	}()
	select {
	case f := <-ch:
		return f.ip, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gatewayOnBoundSubnet is the first routing-table gateway inside the subnet
// of the address a dial controlled by fn goes out from.
func gatewayOnBoundSubnet(fn control.Func) (net.IP, error) {
	subnet, err := boundSubnet(fn)
	if err != nil {
		return nil, err
	}
	gws, err := discoverGateways()
	if err != nil {
		return nil, err
	}
	for _, gw := range gws {
		if gw.To4() != nil && subnet.Contains(gw) {
			return gw, nil
		}
	}
	return nil, fmt.Errorf("no gateway in %s", subnet)
}

// boundSubnet is the network of the interface fn binds to: the only place a
// bound request to the gateway may go.
func boundSubnet(fn control.Func) (*net.IPNet, error) {
	_, subnet, err := boundNetwork(fn)
	return subnet, err
}

// boundNetwork is the address a dial controlled by fn goes out from, and its
// network.
func boundNetwork(fn control.Func) (net.IP, *net.IPNet, error) {
	local, err := boundLocalIP(fn)
	if err != nil {
		return nil, nil, err
	}
	ip := net.ParseIP(local)
	subnet, err := subnetOf(ip)
	if err != nil {
		return nil, nil, err
	}
	return ip, subnet, nil
}

// subnetOf is the network of the interface address ip, found through
// interfaceAddrs.
func subnetOf(ip net.IP) (*net.IPNet, error) {
	if ip == nil {
		return nil, errors.New("no local address")
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return &net.IPNet{IP: n.IP.Mask(n.Mask), Mask: n.Mask}, nil
		}
	}
	return nil, fmt.Errorf("no interface holds %s", ip)
}

// boundLocalIP is the address a dial controlled by fn goes out from. A var so
// tests can supply it.
var boundLocalIP = func(fn control.Func) (string, error) {
	return localIPByDial(&net.Dialer{Control: fn})
}

// interfaceAddrs is net.InterfaceAddrs, a var so tests can supply addresses.
var interfaceAddrs = net.InterfaceAddrs

// discoverPMP finds a gateway that speaks PCP or NAT-PMP, preferring PCP. It
// asks both at once, bounded by pmpProbeTimeout. gw is the gateway's address,
// or nil to find the default gateway.
func discoverPMP(ctx context.Context, gw *net.UDPAddr) (igdClient, string, error) {
	return discoverPMPBound(ctx, gw, nil)
}

// discoverPMPBound is discoverPMP with every request's socket, and the
// returned client's, controlled by fn when it is set (see BoundTo). The
// gateway it finds is then the one on fn's interface.
func discoverPMPBound(ctx context.Context, gw *net.UDPAddr, fn control.Func) (igdClient, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	// The gateway lookup counts against the probe bound too: on Windows it
	// runs route print, which takes no context.
	bounded, cancel := context.WithTimeout(ctx, pmpProbeTimeout)
	defer cancel()
	if gw == nil {
		ip, err := findGatewayBound(bounded, fn)
		if err != nil {
			return nil, "", fmt.Errorf("find default gateway: %w", err)
		}
		gw = &net.UDPAddr{IP: ip, Port: pmpServerPort}
	}
	d, _ := bounded.Deadline()
	timeout := time.Until(d)
	pcp := newPCPClient(gw, timeout)
	pmp := newNATPMPClient(gw, timeout)
	pcp.control, pmp.control = fn, fn
	pcpErr := make(chan error, 1)
	pmpErr := make(chan error, 1)
	go func() { pcpErr <- pcp.announce() }()
	go func() {
		_, err := pmp.GetExternalIPAddress()
		pmpErr <- err
	}()
	// Probes still running when this returns end within their timeout, and
	// their results are dropped.
	var errPCP, errNATPMP error
	pcpDone, pmpDone := false, false
wait:
	for !pcpDone || !pmpDone {
		select {
		case errPCP = <-pcpErr:
			pcpDone = true
			if errPCP == nil {
				// PCP is preferred, so NAT-PMP's answer doesn't matter.
				break wait
			}
		case errNATPMP = <-pmpErr:
			pmpDone = true
		case <-bounded.Done():
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			// The probe bound ended: keep whichever answered.
			if !pcpDone {
				errPCP = fmt.Errorf("pcp: %w", bounded.Err())
			}
			if !pmpDone {
				errNATPMP = fmt.Errorf("nat-pmp: %w", bounded.Err())
			}
			break wait
		}
	}
	if pcpDone && errPCP == nil {
		pcp.timeout = pmpTimeout
		return pcp, "pcp", nil
	}
	if pmpDone && errNATPMP == nil {
		pmp.timeout = pmpTimeout
		return pmp, "nat-pmp", nil
	}
	return nil, "", errors.Join(errPCP, errNATPMP)
}
