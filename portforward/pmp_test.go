package portforward

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePMPGateway answers NAT-PMP or PCP requests on a localhost port. A
// gateway speaking one protocol answers the other's requests with its own
// version and an unsupported-version code, as RFC 6887 asks.
type fakePMPGateway struct {
	t    *testing.T
	conn *net.UDPConn
	pcp  bool

	mu sync.Mutex
	// assign, if set, is the external port the gateway assigns instead of the
	// one asked for.
	assign uint16
	// result is the result code it answers with.
	result byte
	// silent drops every request.
	silent bool
	// maps records each MAP request: protocol, internal port, lifetime.
	maps []fakeMap
	// nonces records the nonce of each PCP MAP request.
	nonces [][12]byte
}

type fakeMap struct {
	protocol     string
	internalPort uint16
	lifetime     uint32
}

func newFakePMPGateway(t *testing.T, pcp bool) *fakePMPGateway {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	g := &fakePMPGateway{t: t, conn: conn, pcp: pcp}
	t.Cleanup(func() { conn.Close() })
	go g.serve()
	return g
}

func (g *fakePMPGateway) addr() *net.UDPAddr { return g.conn.LocalAddr().(*net.UDPAddr) }

func (g *fakePMPGateway) serve() {
	buf := make([]byte, 1100)
	for {
		n, from, err := g.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g.mu.Lock()
		res := g.answer(buf[:n])
		g.mu.Unlock()
		if res != nil {
			_, _ = g.conn.WriteToUDP(res, from)
		}
	}
}

// answer builds the response to req. g.mu is held.
func (g *fakePMPGateway) answer(req []byte) []byte {
	if g.silent || len(req) < 2 {
		return nil
	}
	if g.pcp {
		if req[0] != pcpVersion {
			// Unsupported version, answered as PCP.
			res := make([]byte, pcpHeaderLen)
			res[0], res[1], res[3] = pcpVersion, 0x80|req[1], 1
			return res
		}
		res := make([]byte, pcpHeaderLen, pcpHeaderLen+pcpMapLen)
		res[0], res[1], res[3] = pcpVersion, 0x80|req[1], g.result
		copy(res[4:8], req[4:8])
		if req[1] == pcpOpMap {
			m := append([]byte(nil), req[pcpHeaderLen:pcpHeaderLen+pcpMapLen]...)
			proto := TCP
			if m[12] == pcpProtocolUDP {
				proto = UDP
			}
			g.maps = append(g.maps, fakeMap{proto, binary.BigEndian.Uint16(m[16:18]), binary.BigEndian.Uint32(req[4:8])})
			g.nonces = append(g.nonces, [12]byte(m[0:12]))
			if g.assign != 0 {
				binary.BigEndian.PutUint16(m[18:20], g.assign)
			}
			copy(m[20:36], net.IPv4(203, 0, 113, 7).To16())
			res = append(res, m...)
		}
		return res
	}
	if req[0] != natPMPVersion {
		// A NAT-PMP-only gateway answers PCP with version 0, result 1.
		return []byte{natPMPVersion, 0x80 | req[1], 0, 1}
	}
	op := req[1]
	res := make([]byte, 16)
	res[0], res[1] = natPMPVersion, 128+op
	binary.BigEndian.PutUint16(res[2:4], uint16(g.result))
	switch op {
	case natPMPOpExternalIP:
		copy(res[8:12], net.IPv4(203, 0, 113, 8).To4())
		return res[:12]
	case natPMPOpMapTCP, natPMPOpMapUDP:
		proto := TCP
		if op == natPMPOpMapUDP {
			proto = UDP
		}
		internal := binary.BigEndian.Uint16(req[4:6])
		lifetime := binary.BigEndian.Uint32(req[8:12])
		g.maps = append(g.maps, fakeMap{proto, internal, lifetime})
		copy(res[8:10], req[4:6])
		copy(res[10:12], req[6:8])
		if g.assign != 0 && lifetime > 0 {
			binary.BigEndian.PutUint16(res[10:12], g.assign)
		}
		copy(res[12:16], req[8:12])
		return res
	}
	return nil
}

// set changes the gateway's behaviour while it serves.
func (g *fakePMPGateway) set(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f()
}

func (g *fakePMPGateway) recorded() []fakeMap {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]fakeMap(nil), g.maps...)
}

func TestNATPMP_MapAndDelete(t *testing.T) {
	g := newFakePMPGateway(t, false)
	c := natPMPClient{gw: g.addr(), timeout: time.Second}

	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
	require.NoError(t, c.AddPortMapping("", 30001, TCP, 30001, "", true, "test", 3600))
	require.NoError(t, c.DeletePortMapping("", 41641, UDP))
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {TCP, 30001, 3600}, {UDP, 41641, 0}}, g.recorded())

	ip, err := c.GetExternalIPAddress()
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.8", ip)
}

// A gateway that assigns another external port has the mapping removed and
// the add refused, because callers advertise the internal port.
func TestNATPMP_WrongPortIsRemovedAndRefused(t *testing.T) {
	g := newFakePMPGateway(t, false)
	g.set(func() { g.assign = 50000 })
	c := natPMPClient{gw: g.addr(), timeout: time.Second}

	err := c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600)
	assert.ErrorContains(t, err, "assigned external port 50000")
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 0}}, g.recorded())
}

func TestNATPMP_ResultCodeIsAnError(t *testing.T) {
	g := newFakePMPGateway(t, false)
	g.set(func() { g.result = 2 })
	c := natPMPClient{gw: g.addr(), timeout: time.Second}
	assert.ErrorContains(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600), "result code 2")
}

func TestPCP_MapRenewAndDeleteShareANonce(t *testing.T) {
	g := newFakePMPGateway(t, true)
	c := newPCPClient(g.addr(), time.Second)

	_, err := c.GetExternalIPAddress()
	assert.Error(t, err, "no mapping has reported an address yet")

	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test (renew)", 3600))
	require.NoError(t, c.DeletePortMapping("", 41641, UDP))
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 3600}, {UDP, 41641, 0}}, g.recorded())

	g.mu.Lock()
	nonces := g.nonces
	g.mu.Unlock()
	require.Len(t, nonces, 3)
	assert.Equal(t, nonces[0], nonces[1], "a renewal must reuse the mapping's nonce")
	assert.Equal(t, nonces[0], nonces[2], "a delete must reuse the mapping's nonce")

	ip, err := c.GetExternalIPAddress()
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)

	// A new mapping after the delete gets a fresh nonce.
	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
	g.mu.Lock()
	assert.NotEqual(t, nonces[0], g.nonces[3])
	g.mu.Unlock()
}

func TestPCP_WrongPortIsRemovedAndRefused(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.assign = 50000 })
	c := newPCPClient(g.addr(), time.Second)

	err := c.AddPortMapping("", 30001, TCP, 30001, "", true, "test", 3600)
	assert.ErrorContains(t, err, "assigned external port 50000")
	assert.Equal(t, []fakeMap{{TCP, 30001, 3600}, {TCP, 30001, 0}}, g.recorded())
}

func TestDiscoverPMP_PrefersPCP(t *testing.T) {
	g := newFakePMPGateway(t, true)
	c, method, err := discoverPMP(context.Background(), g.addr())
	require.NoError(t, err)
	assert.Equal(t, "pcp", method)
	assert.IsType(t, &pcpClient{}, c)
	assert.Equal(t, pmpTimeout, c.(*pcpClient).timeout, "requests after discovery get the full timeout")
}

func TestDiscoverPMP_FallsBackToNATPMP(t *testing.T) {
	g := newFakePMPGateway(t, false)
	c, method, err := discoverPMP(context.Background(), g.addr())
	require.NoError(t, err)
	assert.Equal(t, "nat-pmp", method)
	assert.IsType(t, natPMPClient{}, c)
}

// A gateway that ignores both protocols costs discovery pmpProbeTimeout, not
// the caller's whole budget, so UPnP still has time after it.
func TestDiscoverPMP_SilentGatewayIsBounded(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.silent = true })
	start := time.Now()
	_, _, err := discoverPMP(context.Background(), g.addr())
	assert.Error(t, err)
	assert.Less(t, time.Since(start), pmpProbeTimeout+500*time.Millisecond)
}

// With nothing listening, the port-unreachable answer ends discovery at once.
func TestDiscoverPMP_NothingListening(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	addr := conn.LocalAddr().(*net.UDPAddr)
	conn.Close()
	start := time.Now()
	_, _, err = discoverPMP(context.Background(), addr)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), pmpProbeTimeout)
}

// A Forwarder on PCP maps, renews and unmaps through the same paths as UPnP.
func TestForwarder_OnPCP(t *testing.T) {
	g := newFakePMPGateway(t, true)
	f := &Forwarder{client: newPCPClient(g.addr(), time.Second), method: "pcp"}

	m, err := f.MapPort(context.Background(), UDP, 41641, "Lantern node")
	require.NoError(t, err)
	assert.Equal(t, "pcp", m.Method)
	ip, err := f.ExternalIP(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", ip)
	require.NoError(t, f.UnmapPort(context.Background()))
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 0}}, g.recorded())
}
