package portforward

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackpal/gateway"
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
)

// errPMPVersion is returned when the gateway answers in the other protocol's
// version, so discovery can move on at once rather than wait out the timeout.
var errPMPVersion = errors.New("gateway does not speak this protocol version")

// pmpExchange sends the request build returns to gw, resending on the RFCs'
// schedule, and returns the first response accept takes, or accept's error.
// build gets the socket's local address, which PCP puts in its requests.
func pmpExchange(gw *net.UDPAddr, timeout time.Duration, build func(local net.IP) []byte, accept func(b []byte) (bool, error)) ([]byte, error) {
	conn, err := net.DialUDP("udp4", nil, gw)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := build(conn.LocalAddr().(*net.UDPAddr).IP)
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1100)
	for wait := pmpFirstResend; ; wait *= 2 {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		next := time.Now().Add(wait)
		if next.After(deadline) {
			next = deadline
		}
		if err := conn.SetReadDeadline(next); err != nil {
			return nil, err
		}
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					// Typically ICMP port unreachable: nothing listens on 5351.
					return nil, err
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
			return nil, fmt.Errorf("no answer from %s within %s", gw, timeout)
		}
	}
}

// natPMPClient speaks NAT-PMP. The gateway maps to whichever host sent the
// request, so internalClient is unused.
type natPMPClient struct {
	gw      *net.UDPAddr
	timeout time.Duration
}

const (
	natPMPVersion      = 0
	natPMPOpExternalIP = 0
	natPMPOpMapUDP     = 1
	natPMPOpMapTCP     = 2
)

func (c natPMPClient) request(op byte, req []byte, minLen int) ([]byte, error) {
	res, err := pmpExchange(c.gw, c.timeout, func(net.IP) []byte { return req },
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

// mapPort asks for a mapping and returns the external port the gateway
// assigned. A lifetime of 0 deletes the mapping for internalPort.
func (c natPMPClient) mapPort(protocol string, internalPort, externalPort uint16, lifetime uint32) (uint16, error) {
	op := byte(natPMPOpMapTCP)
	if protocol == UDP {
		op = natPMPOpMapUDP
	}
	req := make([]byte, 12)
	req[0], req[1] = natPMPVersion, op
	binary.BigEndian.PutUint16(req[4:6], internalPort)
	binary.BigEndian.PutUint16(req[6:8], externalPort)
	binary.BigEndian.PutUint32(req[8:12], lifetime)
	res, err := c.request(op, req, 16)
	if err != nil {
		return 0, err
	}
	if got := binary.BigEndian.Uint16(res[8:10]); got != internalPort {
		return 0, fmt.Errorf("nat-pmp: answer is for internal port %d, not %d", got, internalPort)
	}
	return binary.BigEndian.Uint16(res[10:12]), nil
}

func (c natPMPClient) AddPortMapping(_ string, externalPort uint16, protocol string, internalPort uint16, _ string, _ bool, _ string, leaseDuration uint32) error {
	got, err := c.mapPort(protocol, internalPort, externalPort, leaseDuration)
	if err != nil {
		return err
	}
	if got != externalPort {
		if _, err := c.mapPort(protocol, internalPort, 0, 0); err != nil {
			slog.Warn("portforward: could not remove a mapping on the wrong port", "method", "nat-pmp", "err", err)
		}
		return fmt.Errorf("nat-pmp: gateway assigned external port %d, not %d", got, externalPort)
	}
	return nil
}

// DeletePortMapping deletes by internal port, which NAT-PMP keys mappings on.
// AddPortMapping only keeps mappings whose external port equals it.
func (c natPMPClient) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	_, err := c.mapPort(protocol, externalPort, 0, 0)
	return err
}

func (c natPMPClient) GetExternalIPAddress() (string, error) {
	res, err := c.request(natPMPOpExternalIP, []byte{natPMPVersion, natPMPOpExternalIP}, 12)
	if err != nil {
		return "", err
	}
	return net.IP(res[8:12]).String(), nil
}

// pcpClient speaks PCP. Refreshing or deleting a mapping needs the nonce that
// created it, so the client keeps one per protocol and internal port. PCP has
// no request for the external address alone, so the client remembers the one
// its last mapping reported.
type pcpClient struct {
	gw      *net.UDPAddr
	timeout time.Duration

	mu         sync.Mutex
	nonces     map[pcpKey][12]byte
	externalIP net.IP
}

type pcpKey struct {
	protocol     string
	internalPort uint16
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
	return &pcpClient{gw: gw, timeout: timeout, nonces: map[pcpKey][12]byte{}}
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

// pcpAccept takes a response to op, and returns check's verdict on it once its
// result code is a success.
func pcpAccept(op byte, check func(b []byte) bool) func(b []byte) (bool, error) {
	return func(b []byte) (bool, error) {
		if len(b) >= 1 && b[0] != pcpVersion {
			return false, errPMPVersion
		}
		if len(b) < pcpHeaderLen || b[1] != 0x80|op {
			return false, nil
		}
		if !check(b) {
			return false, nil
		}
		if b[3] != 0 {
			return false, fmt.Errorf("PCP result code %d", b[3])
		}
		return true, nil
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

// mapPort sends a MAP request and returns the external port the gateway
// assigned. A lifetime of 0 deletes the mapping.
func (c *pcpClient) mapPort(protocol string, internalPort, externalPort uint16, lifetime uint32) (uint16, error) {
	nonce, err := c.nonce(protocol, internalPort)
	if err != nil {
		return 0, err
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
	res, err := pmpExchange(c.gw, c.timeout, build, pcpAccept(pcpOpMap, func(b []byte) bool {
		if len(b) < pcpHeaderLen+pcpMapLen {
			return false
		}
		m := b[pcpHeaderLen:]
		return [12]byte(m[0:12]) == nonce && m[12] == proto && binary.BigEndian.Uint16(m[16:18]) == internalPort
	}))
	if err != nil {
		return 0, fmt.Errorf("pcp: %w", err)
	}
	m := res[pcpHeaderLen:]
	if lifetime > 0 {
		c.mu.Lock()
		c.externalIP = net.IP(append([]byte(nil), m[20:36]...))
		c.mu.Unlock()
	} else {
		c.mu.Lock()
		delete(c.nonces, pcpKey{protocol, internalPort})
		c.mu.Unlock()
	}
	return binary.BigEndian.Uint16(m[18:20]), nil
}

func (c *pcpClient) AddPortMapping(_ string, externalPort uint16, protocol string, internalPort uint16, _ string, _ bool, _ string, leaseDuration uint32) error {
	got, err := c.mapPort(protocol, internalPort, externalPort, leaseDuration)
	if err != nil {
		return err
	}
	if got != externalPort {
		if _, err := c.mapPort(protocol, internalPort, got, 0); err != nil {
			slog.Warn("portforward: could not remove a mapping on the wrong port", "method", "pcp", "err", err)
		}
		return fmt.Errorf("pcp: gateway assigned external port %d, not %d", got, externalPort)
	}
	return nil
}

// DeletePortMapping deletes by internal port, as the mapping's nonce is keyed.
// AddPortMapping only keeps mappings whose external port equals it.
func (c *pcpClient) DeletePortMapping(_ string, externalPort uint16, protocol string) error {
	_, err := c.mapPort(protocol, externalPort, externalPort, 0)
	return err
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
	_, err := pmpExchange(c.gw, c.timeout, func(local net.IP) []byte { return pcpHeader(pcpOpAnnounce, 0, local) },
		pcpAccept(pcpOpAnnounce, func([]byte) bool { return true }))
	if err != nil {
		return fmt.Errorf("pcp: %w", err)
	}
	return nil
}

// discoverPMP finds a gateway that speaks PCP or NAT-PMP, preferring PCP. It
// asks both at once, bounded by pmpProbeTimeout. gw is the gateway's address,
// or nil to find the default gateway.
func discoverPMP(ctx context.Context, gw *net.UDPAddr) (igdClient, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if gw == nil {
		ip, err := gateway.DiscoverGateway()
		if err != nil {
			return nil, "", fmt.Errorf("find default gateway: %w", err)
		}
		gw = &net.UDPAddr{IP: ip, Port: pmpServerPort}
	}
	timeout := pmpProbeTimeout
	if d, ok := ctx.Deadline(); ok && time.Until(d) < timeout {
		timeout = time.Until(d)
	}
	pcp := newPCPClient(gw, timeout)
	pmp := natPMPClient{gw: gw, timeout: timeout}
	pcpErr := make(chan error, 1)
	pmpErr := make(chan error, 1)
	go func() { pcpErr <- pcp.announce() }()
	go func() {
		_, err := pmp.GetExternalIPAddress()
		pmpErr <- err
	}()
	errPCP, errNATPMP := <-pcpErr, <-pmpErr
	if errPCP == nil {
		pcp.timeout = pmpTimeout
		return pcp, "pcp", nil
	}
	if errNATPMP == nil {
		pmp.timeout = pmpTimeout
		return pmp, "nat-pmp", nil
	}
	return nil, "", errors.Join(errPCP, errNATPMP)
}
