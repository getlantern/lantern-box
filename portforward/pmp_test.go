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
	// dropAdds drops the answer to every MAP request that isn't a delete,
	// after recording it, as when the gateway acts but its answer is lost.
	dropAdds bool
	// grant, if set, is the lifetime the gateway grants instead of the one
	// asked for.
	grant uint32
	// onMap, if set, sees each NAT-PMP MAP request.
	onMap func(req []byte)
	// headerOnlyErrors makes PCP error answers carry only the common header.
	headerOnlyErrors bool
	// ignoreV0 makes a PCP gateway drop NAT-PMP requests instead of answering
	// them with an unsupported-version error.
	ignoreV0 bool
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
			if g.ignoreV0 {
				return nil
			}
			// Unsupported version, answered as PCP.
			res := make([]byte, pcpHeaderLen)
			res[0], res[1], res[3] = pcpVersion, 0x80|req[1], 1
			return res
		}
		res := make([]byte, pcpHeaderLen, pcpHeaderLen+pcpMapLen)
		res[0], res[1], res[3] = pcpVersion, 0x80|req[1], g.result
		copy(res[4:8], req[4:8])
		if g.grant != 0 && binary.BigEndian.Uint32(req[4:8]) > 0 {
			binary.BigEndian.PutUint32(res[4:8], g.grant)
		}
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
			if g.dropAdds && binary.BigEndian.Uint32(req[4:8]) > 0 {
				return nil
			}
			if g.headerOnlyErrors && g.result != 0 {
				return res[:pcpHeaderLen]
			}
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
		if g.onMap != nil {
			g.onMap(req)
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
		if g.grant != 0 && lifetime > 0 {
			binary.BigEndian.PutUint32(res[12:16], g.grant)
		}
		if g.dropAdds && lifetime > 0 {
			return nil
		}
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

// collapse drops consecutive repeats, which are a request's resends.
func collapse(ms []fakeMap) []fakeMap {
	var out []fakeMap
	for _, m := range ms {
		if len(out) == 0 || out[len(out)-1] != m {
			out = append(out, m)
		}
	}
	return out
}

func (g *fakePMPGateway) recorded() []fakeMap {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]fakeMap(nil), g.maps...)
}

func TestNATPMP_MapAndDelete(t *testing.T) {
	g := newFakePMPGateway(t, false)
	c := newNATPMPClient(g.addr(), time.Second)

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
	c := newNATPMPClient(g.addr(), time.Second)

	err := c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600)
	assert.ErrorContains(t, err, "assigned external port 50000")
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 0}}, g.recorded())
}

func TestNATPMP_ResultCodeIsAnError(t *testing.T) {
	g := newFakePMPGateway(t, false)
	g.set(func() { g.result = 2 })
	c := newNATPMPClient(g.addr(), time.Second)
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

	// The nonce outlives the delete: a renewal racing teardown can re-create
	// the mapping, and only the same nonce can then remove it.
	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
	require.NoError(t, c.DeletePortMapping("", 41641, UDP))
	g.mu.Lock()
	assert.Equal(t, nonces[0], g.nonces[3])
	assert.Equal(t, nonces[0], g.nonces[4])
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
	assert.IsType(t, &natPMPClient{}, c)
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

// A NAT-PMP deletion asks for external port 0, as RFC 6886 §3.4 requires.
func TestNATPMP_DeleteSuggestsPortZero(t *testing.T) {
	g := newFakePMPGateway(t, false)
	var seen []uint16
	g.set(func() { g.onMap = func(req []byte) { seen = append(seen, binary.BigEndian.Uint16(req[6:8])) } })
	c := newNATPMPClient(g.addr(), time.Second)
	require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
	require.NoError(t, c.DeletePortMapping("", 41641, UDP))
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Equal(t, []uint16{41641, 0}, seen)
}

// An add whose answer never comes is deleted, since the gateway may have made
// it and the Forwarder won't track it. A renewal whose answer never comes is
// left for teardown, since deleting it would remove a live mapping.
func TestPMP_LostAnswer(t *testing.T) {
	for _, pcp := range []bool{false, true} {
		g := newFakePMPGateway(t, pcp)
		var c igdClient = newNATPMPClient(g.addr(), 300*time.Millisecond)
		if pcp {
			c = newPCPClient(g.addr(), 300*time.Millisecond)
		}
		g.set(func() { g.dropAdds = true })
		err := c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600)
		assert.ErrorIs(t, err, errNoAnswer, "pcp=%v", pcp)
		assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 0}}, collapse(g.recorded()), "pcp=%v: a fresh add whose answer was lost must be deleted", pcp)

		g.set(func() { g.dropAdds = false; g.maps = nil })
		require.NoError(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600))
		g.set(func() { g.dropAdds = true })
		assert.ErrorIs(t, c.AddPortMapping("", 41641, UDP, 41641, "", true, "test (renew)", 3600), errNoAnswer)
		assert.Equal(t, []fakeMap{{UDP, 41641, 3600}}, collapse(g.recorded()), "pcp=%v: a renewal must not be deleted", pcp)
	}
}

// A gateway that grants a shorter lease than asked sets the mapping's lease,
// and renewal comes at half of it.
func TestForwarder_RenewsWithinTheGrantedLease(t *testing.T) {
	for _, pcp := range []bool{false, true} {
		g := newFakePMPGateway(t, pcp)
		g.set(func() { g.grant = 120 })
		var c igdClient = newNATPMPClient(g.addr(), time.Second)
		if pcp {
			c = newPCPClient(g.addr(), time.Second)
		}
		f := &Forwarder{client: c, method: "test"}
		m, err := f.MapPort(context.Background(), UDP, 41641, "test")
		require.NoError(t, err)
		assert.Equal(t, 120*time.Second, m.LeaseDuration, "pcp=%v", pcp)
		assert.InDelta(t, 60*time.Second, f.nextRenewal(30*time.Minute), float64(time.Second), "pcp=%v", pcp)

		// A renewal asks for the original lease again, and records what it got.
		g.set(func() { g.grant = 600 })
		require.True(t, f.renew(f.mappings[UDP], c))
		assert.Equal(t, uint32(3600), g.recorded()[1].lifetime, "pcp=%v", pcp)
		assert.InDelta(t, 300*time.Second, f.nextRenewal(30*time.Minute), float64(time.Second), "pcp=%v", pcp)

		// A renewal granted next to nothing is kept, and renewed at the floor.
		g.set(func() { g.grant = 1 })
		require.True(t, f.renew(f.mappings[UDP], c))
		assert.Equal(t, minRenewInterval, renewPeriod(f.mappings[UDP], 30*time.Minute), "pcp=%v", pcp)
	}
}

// A new mapping granted only seconds is removed and refused, rather than
// renewed every second or two.
func TestPMP_RefusesATooShortFreshLease(t *testing.T) {
	for _, pcp := range []bool{false, true} {
		g := newFakePMPGateway(t, pcp)
		g.set(func() { g.grant = 5 })
		var c igdClient = newNATPMPClient(g.addr(), time.Second)
		if pcp {
			c = newPCPClient(g.addr(), time.Second)
		}
		err := c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600)
		assert.ErrorContains(t, err, "lease of only 5s", "pcp=%v", pcp)
		assert.Equal(t, []fakeMap{{UDP, 41641, 3600}, {UDP, 41641, 0}}, collapse(g.recorded()), "pcp=%v", pcp)
	}
}

func TestRenewPeriod(t *testing.T) {
	m := &Mapping{LeaseDuration: time.Hour}
	assert.Equal(t, 30*time.Minute, renewPeriod(m, 30*time.Minute))
	assert.Equal(t, 20*time.Millisecond, renewPeriod(m, 20*time.Millisecond), "a shorter interval wins")
	m.LeaseDuration = 120 * time.Second
	assert.Equal(t, 60*time.Second, renewPeriod(m, 30*time.Minute))
	m.LeaseDuration = 0
	assert.Equal(t, minRenewInterval, renewPeriod(m, 30*time.Minute))
}

// A mapping added while renewal waits on a long lease reschedules it, so a
// short grant is renewed in time.
func TestForwarder_LateShortGrantReschedulesRenewal(t *testing.T) {
	g := newFakePMPGateway(t, true)
	c := newPCPClient(g.addr(), time.Second)
	f := &Forwarder{client: c, method: "pcp"}
	_, err := f.MapPort(context.Background(), TCP, 30001, "tcp")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	// TCP alone would next be renewed in an hour.
	go func() { f.renewLoop(ctx, time.Hour); close(done) }()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.wake != nil
	}, time.Second, time.Millisecond)

	// A fresh grant must be at least minGrantedLease, so ask for 10s and get it.
	g.set(func() { g.grant = minGrantedLease })
	_, err = f.MapPort(context.Background(), UDP, 41641, "udp")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		ms := collapse(g.recorded())
		return len(ms) >= 3 && ms[len(ms)-1].protocol == UDP
	}, 2*renewPeriod(&Mapping{LeaseDuration: minGrantedLease * time.Second}, time.Hour)+2*time.Second, 50*time.Millisecond,
		"the UDP mapping, granted %ds, was not renewed in time", minGrantedLease)
	cancel()
	<-done
}

// A renewal whose answer is lost while teardown runs may have re-created the
// mapping, so it is removed like one that timed out.
func TestForwarder_LostRenewalAnswerDuringTeardownIsRemoved(t *testing.T) {
	g := newFakePMPGateway(t, true)
	c := newPCPClient(g.addr(), 300*time.Millisecond)
	f := &Forwarder{client: c, method: "pcp"}
	_, err := f.MapPort(context.Background(), UDP, 41641, "udp")
	require.NoError(t, err)

	g.set(func() { g.dropAdds = true })
	f.mu.Lock()
	m := f.mappings[UDP]
	f.closed = true // teardown has begun
	f.mu.Unlock()
	assert.False(t, f.renew(m, c))
	ms := collapse(g.recorded())
	assert.Equal(t, fakeMap{UDP, 41641, 0}, ms[len(ms)-1], "the possibly re-created mapping must be deleted")
}

// A PCP refusal carrying only the common header is a refusal, not a lost
// answer, so it fails at once and sends no compensating delete.
func TestPCP_HeaderOnlyErrorIsARefusal(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.result = 8; g.headerOnlyErrors = true })
	c := newPCPClient(g.addr(), time.Second)
	start := time.Now()
	err := c.AddPortMapping("", 41641, UDP, 41641, "", true, "test", 3600)
	assert.ErrorContains(t, err, "result code 8")
	assert.NotErrorIs(t, err, errNoAnswer)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
	assert.Equal(t, []fakeMap{{UDP, 41641, 3600}}, collapse(g.recorded()))
}

// Cancelling discovery returns at once, with no client, even while the probes
// wait on a silent gateway.
func TestDiscoverPMP_HonoursCancellation(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.silent = true })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	c, _, err := discoverPMP(ctx, g.addr())
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, c)
	assert.Less(t, time.Since(start), pmpProbeTimeout/2)
}

// The renewal interval comes from the lease asked for, so a short first grant
// doesn't cap it once the gateway grants more.
func TestForwarder_StartRenewal_UsesTheRequestedLease(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.grant = 120 })
	f := &Forwarder{client: newPCPClient(g.addr(), time.Second), method: "pcp"}
	_, err := f.MapPort(context.Background(), UDP, 41641, "udp")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.StartRenewal(ctx)

	g.set(func() { g.grant = 3600 })
	f.mu.Lock()
	m := f.mappings[UDP]
	f.mu.Unlock()
	require.True(t, f.renew(m, f.client))
	interval := baseRenewInterval(m)
	assert.Equal(t, 30*time.Minute, interval, "the loop's interval comes from the 3600s asked for, not the first 120s grant")
	assert.InDelta(t, 30*time.Minute, f.nextRenewal(interval), float64(time.Second))
}

func TestBaseRenewInterval(t *testing.T) {
	assert.Equal(t, 30*time.Minute, baseRenewInterval(&Mapping{requested: 3600, LeaseDuration: 120 * time.Second}))
	assert.Equal(t, time.Minute, baseRenewInterval(&Mapping{requested: 30}))
}

// A gateway lookup that hangs, as route print can on Windows, can't hold
// discovery past its bound.
func TestDiscoverPMP_BoundsTheGatewayLookup(t *testing.T) {
	prev := findGateway
	release := make(chan struct{})
	t.Cleanup(func() { close(release); findGateway = prev })
	findGateway = func(ctx context.Context) (net.IP, error) {
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	start := time.Now()
	_, _, err := discoverPMP(context.Background(), nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), pmpProbeTimeout+500*time.Millisecond)
}

// A gateway that answers PCP at once is used at once, even when NAT-PMP's
// probe would wait out the bound.
func TestDiscoverPMP_PCPAnswerDoesNotWaitForNATPMP(t *testing.T) {
	g := newFakePMPGateway(t, true)
	// The fake answers NAT-PMP's version-0 request with an unsupported-version
	// error, which ends that probe at once; make it silent instead.
	g.set(func() { g.ignoreV0 = true })
	start := time.Now()
	_, method, err := discoverPMP(context.Background(), g.addr())
	require.NoError(t, err)
	assert.Equal(t, "pcp", method)
	assert.Less(t, time.Since(start), pmpProbeTimeout/2)
}

// A failed renewal doesn't move the lease's deadline: it is retried soon, and
// well before a 120s grant runs out.
func TestForwarder_FailedRenewalRetriesWithinTheLease(t *testing.T) {
	g := newFakePMPGateway(t, true)
	g.set(func() { g.grant = 120 })
	c := newPCPClient(g.addr(), 300*time.Millisecond)
	f := &Forwarder{client: c, method: "pcp"}
	_, err := f.MapPort(context.Background(), UDP, 41641, "udp")
	require.NoError(t, err)
	// Due now: added half the 120s grant ago.
	f.mu.Lock()
	m := f.mappings[UDP]
	m.renewed = m.renewed.Add(-60 * time.Second)
	renewed := m.renewed
	f.mu.Unlock()

	g.set(func() { g.dropAdds = true })
	require.True(t, f.renew(m, c))
	f.mu.Lock()
	assert.Equal(t, renewed, m.renewed, "a failed renewal must not move the deadline")
	f.mu.Unlock()
	assert.InDelta(t, 15*time.Second, f.nextRenewal(30*time.Minute), float64(time.Second),
		"retry an eighth of the lease later")

	g.set(func() { g.dropAdds = false })
	require.True(t, f.renew(m, c))
	assert.InDelta(t, 60*time.Second, f.nextRenewal(30*time.Minute), float64(time.Second),
		"a confirmed renewal resets the schedule")
}

func TestRenewRetryDelay(t *testing.T) {
	assert.Equal(t, 15*time.Second, renewRetryDelay(&Mapping{LeaseDuration: 120 * time.Second}))
	assert.Equal(t, time.Minute, renewRetryDelay(&Mapping{LeaseDuration: time.Hour}))
	assert.Equal(t, time.Second, renewRetryDelay(&Mapping{LeaseDuration: 2 * time.Second}))
}

// An error answer carrying a MAP payload for another request, such as one
// with another nonce, isn't taken as this request's refusal.
func TestPCP_ErrorForAnotherNonceIsIgnored(t *testing.T) {
	c := newPCPClient(&net.UDPAddr{}, time.Second)
	nonce, err := c.nonce(UDP, 41641)
	require.NoError(t, err)
	accept := pcpAccept(pcpOpMap, func(b []byte) bool {
		return [12]byte(b[pcpHeaderLen:pcpHeaderLen+12]) == nonce
	})
	res := make([]byte, pcpHeaderLen+pcpMapLen)
	res[0], res[1], res[3] = pcpVersion, 0x80|pcpOpMap, 8
	ok, err := accept(res)
	assert.False(t, ok)
	assert.NoError(t, err, "an error for another nonce must be ignored")

	copy(res[pcpHeaderLen:], nonce[:])
	_, err = accept(res)
	assert.ErrorContains(t, err, "result code 8")

	_, err = accept(res[:pcpHeaderLen])
	assert.ErrorContains(t, err, "result code 8", "a header-only error is taken as the request's")
}

// A transport error after a request has gone out leaves its outcome unknown,
// so it counts as a lost answer, keeping the underlying error.
func TestPMPExchange_ErrorAfterSendIsUncertain(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	addr := conn.LocalAddr().(*net.UDPAddr)
	conn.Close()
	_, err = pmpExchange(addr, time.Second, func(net.IP) []byte { return []byte{0, 0} },
		func([]byte) (bool, error) { return false, nil })
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoAnswer)
	var opErr *net.OpError
	assert.ErrorAs(t, err, &opErr, "the transport error is kept")
}
