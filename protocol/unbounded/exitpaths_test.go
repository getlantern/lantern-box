//go:build with_quic

package unbounded

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"
)

// placeholderConn is a connected UDP socket as clientcore's ICE packet conn: one peer, and a
// fixed address in place of the real one. Copied from exit/exit_test.go.
type placeholderConn struct{ *net.UDPConn }

var placeholderAddr = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}

func (c placeholderConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.UDPConn.Read(p)
	return n, placeholderAddr, err
}

func (c placeholderConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.UDPConn.Write(p) }

// pathPair returns two packet conns joined to each other, as the two ends of an ICE path.
// Copied from exit/exit_test.go.
func pathPair(t *testing.T) (volunteer, user placeholderConn) {
	t.Helper()
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	_ = a.Close()
	_ = b.Close()
	va, err := net.DialUDP("udp4", a.LocalAddr().(*net.UDPAddr), b.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	vb, err := net.DialUDP("udp4", b.LocalAddr().(*net.UDPAddr), a.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = va.Close(); _ = vb.Close() })
	return placeholderConn{va}, placeholderConn{vb}
}

// trackedConn records whether the user's side closed its end of the path.
type trackedConn struct {
	placeholderConn
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.placeholderConn.Close()
}

// recordingConn records the datagrams the volunteer reads and writes: the wire, both ways.
type recordingConn struct {
	placeholderConn
	mu     sync.Mutex
	reads  [][]byte
	writes [][]byte
}

func (c *recordingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return c.placeholderConn.WriteTo(p, addr)
}

// wire returns every datagram recorded, both ways.
func (c *recordingConn) wire() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(slices.Clone(c.reads), c.writes...)
}

func (c *recordingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.placeholderConn.ReadFrom(p)
	if n > 0 {
		c.mu.Lock()
		c.reads = append(c.reads, append([]byte(nil), p[:n]...))
		c.mu.Unlock()
	}
	return n, addr, err
}

func (c *recordingConn) first() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reads) == 0 {
		return nil
	}
	return c.reads[0]
}

// isQUICv1Initial is copied from test/e2e/hysteria2x_test.go.
func isQUICv1Initial(b []byte) bool {
	return len(b) >= 1200 && b[0]&0xf0 == 0xc0 &&
		b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 1
}

// isQUICv1LongHeader reports whether b reads as any QUIC v1 long-header packet: what a DPI box
// keys on before it derives the Initial's keys, which are public, and reads the SNI.
func isQUICv1LongHeader(b []byte) bool {
	return len(b) >= 5 && b[0]&0xc0 == 0xc0 &&
		b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 1
}

// echoRouter routes TCP to a loopback echo listener, refusing destinations in refuse as a
// reject rule does. Adapted from exit/exit_test.go's dialRouter.
type echoRouter struct {
	echo   string
	refuse string
	routed atomic.Int32

	mu    sync.Mutex
	dests []string
}

// destinations returns the destinations routed so far, in order.
func (r *echoRouter) destinations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.dests)
}

func (r *echoRouter) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *echoRouter) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *echoRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.routed.Add(1)
	r.mu.Lock()
	r.dests = append(r.dests, md.Destination.String())
	r.mu.Unlock()
	if r.refuse != "" && md.Destination.String() == r.refuse {
		_ = N.CloseOnHandshakeFailure(conn, onClose, &rule.RejectedError{Cause: errors.New("refused")})
		return
	}
	go func() {
		up, err := net.Dial("tcp", r.echo)
		if err != nil {
			_ = N.CloseOnHandshakeFailure(conn, onClose, err)
			return
		}
		_ = N.ReportHandshakeSuccess(conn)
		err = bufio.CopyConn(ctx, conn, up)
		_ = conn.Close()
		_ = up.Close()
		onClose(err)
	}()
}

// RoutePacketConnectionEx echoes each packet back to the destination it was sent to.
func (r *echoRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.routed.Add(1)
	r.mu.Lock()
	r.dests = append(r.dests, md.Destination.String())
	r.mu.Unlock()
	go func() {
		defer conn.Close()
		for {
			b := buf.NewPacket()
			dest, err := conn.ReadPacket(b)
			if err == nil {
				err = conn.WritePacket(b, dest)
			} else {
				b.Release()
			}
			if err != nil {
				onClose(err)
				return
			}
		}
	}()
}

func newEchoRouter(t *testing.T) *echoRouter {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return &echoRouter{echo: l.Addr().String()}
}

// volunteerExit is Task 5's real exit, enabled, routing into an echo router.
type volunteerExit struct {
	e *exit.Exit
	r *echoRouter
}

func newVolunteerExit(t *testing.T) *volunteerExit {
	t.Helper()
	e, err := exit.New(context.Background(), sblog.NewNOPFactory().Logger())
	require.NoError(t, err)
	t.Cleanup(e.Close)
	r := newEchoRouter(t)
	e.SetEnabled(true)
	e.SetRouter(exit.Pool, r, time.Now().Add(time.Hour), func() error { return nil })
	return &volunteerExit{e: e, r: r}
}

// session is one served session: the volunteer's end of the path, and the user's.
type session struct {
	volunteer placeholderConn
	user      *trackedConn
	ep        UBClientcore.ExitPath
}

// serveSession serves one session over vConn and returns the user's ExitPath.
func (v *volunteerExit) serveSession(t *testing.T, vConn net.PacketConn, uConn *trackedConn) UBClientcore.ExitPath {
	t.Helper()
	pin, password, ok := v.e.Options().Params(false)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { v.e.Options().Serve(ctx, vConn, password, false); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return UBClientcore.ExitPath{Conn: uConn, CertPin: pin, Password: password}
}

func (v *volunteerExit) session(t *testing.T) *session {
	t.Helper()
	vConn, uConn := pathPair(t)
	u := &trackedConn{placeholderConn: uConn}
	return &session{volunteer: vConn, user: u, ep: v.serveSession(t, vConn, u)}
}

// path serves one session and returns the user's ExitPath.
func (v *volunteerExit) path(t *testing.T) UBClientcore.ExitPath {
	t.Helper()
	return v.session(t).ep
}

// badPinSession serves a session whose ExitPath pins another exit's certificate.
func (v *volunteerExit) badPinSession(t *testing.T) *session {
	t.Helper()
	s := v.session(t)
	other := newVolunteerExit(t)
	pin, _, _ := other.e.Options().Params(false)
	s.ep.CertPin = pin
	return s
}

func testLogger() logger.ContextLogger { return sblog.NewNOPFactory().Logger() }

// testAllowlistJSON admits, over TCP and UDP to any port, example.com and its subdomains, and
// 10.0.0.0/8.
const testAllowlistJSON = `{"type": "inline", "tag": "allowlist", "rules": [
	{"network": ["tcp", "udp"], "domain_suffix": ["example.com"]},
	{"network": ["tcp", "udp"], "ip_cidr": ["10.0.0.0/8"]}
]}`

func testRuleSet(t *testing.T, raw string) *sbOption.RuleSet {
	t.Helper()
	rs, err := json.UnmarshalExtended[sbOption.RuleSet]([]byte(raw))
	require.NoError(t, err)
	return &rs
}

func testAllowlist(t *testing.T, raw string) *poolAllowlist {
	t.Helper()
	a, err := newPoolAllowlist(context.Background(), testLogger(), testRuleSet(t, raw))
	require.NoError(t, err)
	return a
}

// newTestExitPaths takes paths as pool ones, under testAllowlistJSON.
func newTestExitPaths(t *testing.T) *exitPaths {
	t.Helper()
	p := newExitPaths(context.Background(), testLogger(), "www.example.com", testAllowlist(t, testAllowlistJSON))
	require.NotNil(t, p)
	t.Cleanup(p.close)
	return p
}

func dialTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func echoes(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
}

func (p *exitPaths) live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

func TestExitPathsDialThroughTheNewestPath(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	older := v.session(t)
	p.onExitPath(older.ep)
	newer := newVolunteerExit(t)
	p.onExitPath(newer.path(t))
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	assert.EqualValues(t, 1, newer.r.routed.Load(), "the newest path carried the dial")
	assert.Zero(t, v.r.routed.Load())
	assert.Equal(t, 2, p.live())
}

// A newest path whose volunteer never answers costs a dial the QUIC handshake timeout, not
// sing-box's 15s TLS default, before the dial moves to the next path.
func TestExitPathsFailOverFromASilentPath(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.onExitPath(v.path(t))
	_, uConn := pathPair(t)
	pin, _, _ := v.e.Options().Params(false)
	p.onExitPath(UBClientcore.ExitPath{Conn: &trackedConn{placeholderConn: uConn}, CertPin: pin, Password: "x"})
	started := time.Now()
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	assert.Less(t, time.Since(started), 7*time.Second)
	echoes(t, conn)
	assert.EqualValues(t, 1, v.r.routed.Load(), "the older path carried the dial")
	assert.Equal(t, 1, p.live(), "the silent path was dropped")
}

func TestExitPathsNoPath(t *testing.T) {
	p := newTestExitPaths(t)
	_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	assert.ErrorIs(t, err, errNoExitPath)
	_, err = p.listenPacket(dialTimeout(t), M.ParseSocksaddr("example.com:53"))
	assert.ErrorIs(t, err, errNoExitPath)
	assert.True(t, p.takeExit())
}

func withDomain(ctx context.Context, domain string) context.Context {
	return adapter.WithContext(ctx, &adapter.InboundContext{Domain: domain})
}

// The volunteer's exit admits only allowlisted hostnames and rejects IP literals before DNS,
// so a TCP dial to an IP names the host sniffed from the connection instead, with the IP's
// port. Mirrors samizdatreverse's TestReverseSendsSniffedHost.
func TestExitPathsSendTheSniffedHostForAnIP(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.onExitPath(v.path(t))
	for _, tc := range []struct {
		name, domain, dest, want string
	}{
		{"sniffed host for an IPv4", "example.com", "10.11.12.13:443", "example.com:443"},
		{"sniffed host for an IPv6", "example.com", "[2001:db8::1]:8443", "example.com:8443"},
		{"no sniffed host", "", "10.11.12.13:443", "10.11.12.13:443"},
		{"sniffed text that is no domain", "not a domain", "10.11.12.13:443", "10.11.12.13:443"},
		{"a domain destination kept", "other.example.com", "example.com:443", "example.com:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := p.dial(withDomain(dialTimeout(t), tc.domain), "tcp", M.ParseSocksaddr(tc.dest))
			require.NoError(t, err)
			echoes(t, conn)
			dests := v.r.destinations()
			require.NotEmpty(t, dests)
			assert.Equal(t, tc.want, dests[len(dests)-1])
		})
	}
}

// A pool path carries only what its allowlist admits. Anything else takes today's relay path
// (errNoExitPath) without a round trip to the exit, which would refuse it: the volunteer's exit
// box routes nothing.
func TestExitPathsPoolPathTakesOnlyTheAllowlist(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.onExitPath(v.path(t))

	for _, d := range []string{"example.com:80", "www.example.com:443", "10.11.12.13:443"} {
		conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr(d))
		require.NoError(t, err, d)
		echoes(t, conn)
	}
	// An IP whose sniffed host is admitted goes as that host.
	conn, err := p.dial(withDomain(dialTimeout(t), "example.com"), "tcp", M.ParseSocksaddr("192.0.2.1:443"))
	require.NoError(t, err)
	echoes(t, conn)
	routed := v.r.routed.Load()
	require.EqualValues(t, 4, routed)

	for _, tc := range []struct{ network, dest, sniffed string }{
		{"tcp", "other.test:80", ""},
		{"tcp", "notexample.com:443", ""},
		{"tcp", "192.0.2.1:443", ""},
		{"tcp", "192.0.2.1:443", "other.test"},
		// A domain that would resolve into 10.0.0.0/8 is still not admitted: the exit checks
		// before DNS.
		{"tcp", "ten.test:443", ""},
		{"udp", "other.test:443", ""},
		{"udp", "192.0.2.1:443", ""},
	} {
		ctx := withDomain(dialTimeout(t), tc.sniffed)
		_, err := p.dial(ctx, tc.network, M.ParseSocksaddr(tc.dest))
		assert.ErrorIs(t, err, errNoExitPath, "%s %s", tc.network, tc.dest)
		if tc.network == "udp" {
			_, err = p.listenPacket(ctx, M.ParseSocksaddr(tc.dest))
			assert.ErrorIs(t, err, errNoExitPath, "%s %s", tc.network, tc.dest)
		}
	}
	assert.Equal(t, routed, v.r.routed.Load(), "nothing unadmitted reached the exit")
	assert.Zero(t, p.failureCount(), "a relayed connection is no path failure")
	assert.Equal(t, 1, p.live())

	// QUIC to an admitted IP or host goes through the path.
	for _, d := range []string{"10.11.12.13:443", "example.com:443"} {
		pc, err := p.listenPacket(dialTimeout(t), M.ParseSocksaddr(d))
		require.NoError(t, err, d)
		_ = pc.Close()
	}
}

// With no allowlist configured a pool path carries nothing.
func TestExitPathsPoolPathWithoutAnAllowlistTakesNothing(t *testing.T) {
	v := newVolunteerExit(t)
	p := newExitPaths(context.Background(), testLogger(), "www.example.com", nil)
	t.Cleanup(p.close)
	p.onExitPath(v.path(t))
	_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	assert.ErrorIs(t, err, errNoExitPath)
	_, err = p.listenPacket(dialTimeout(t), M.ParseSocksaddr("example.com:443"))
	assert.ErrorIs(t, err, errNoExitPath)
	assert.Zero(t, v.r.routed.Load())
	assert.Zero(t, p.failureCount())
}

// A personal path, under its volunteer's personal policy, carries anything, QUIC to an IP too.
func TestExitPathsPersonalPathTakesAnything(t *testing.T) {
	v := newVolunteerExit(t)
	p := newExitPaths(context.Background(), testLogger(), "www.example.com", nil)
	t.Cleanup(p.close)
	ep := v.path(t)
	ep.Personal = true
	p.onExitPath(ep)
	for _, d := range []string{"other.test:80", "192.0.2.1:443"} {
		conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr(d))
		require.NoError(t, err, d)
		echoes(t, conn)
	}
	pc, err := p.listenPacket(dialTimeout(t), M.ParseSocksaddr("192.0.2.1:443"))
	require.NoError(t, err)
	defer pc.Close()
	_ = pc.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = pc.WriteTo([]byte("quic"), M.ParseSocksaddr("192.0.2.1:443"))
	require.NoError(t, err)
	b := make([]byte, 16)
	n, _, err := pc.ReadFrom(b)
	require.NoError(t, err)
	assert.Equal(t, "quic", string(b[:n]))
	assert.Equal(t, []string{"other.test:80", "192.0.2.1:443", "192.0.2.1:443"}, v.r.destinations())
}

// A live personal path is taken over a newer pool one, even for what the pool path admits.
func TestExitPathsPreferAPersonalPath(t *testing.T) {
	personal, pool := newVolunteerExit(t), newVolunteerExit(t)
	p := newTestExitPaths(t)
	personalEP, poolEP := personal.path(t), pool.path(t)
	personalEP.Personal = true
	p.onExitPath(personalEP)
	p.onExitPath(poolEP)
	for _, d := range []string{"example.com:80", "other.test:80"} {
		conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr(d))
		require.NoError(t, err, d)
		echoes(t, conn)
	}
	assert.EqualValues(t, 2, personal.r.routed.Load())
	assert.Zero(t, pool.r.routed.Load())

	// Once the personal path is gone, the pool path takes what it admits, and only that.
	_ = personalEP.Conn.Close()
	p.mu.Lock()
	for _, path := range p.paths {
		if path.personal {
			_ = path.conn.Close()
		}
	}
	p.mu.Unlock()
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	assert.EqualValues(t, 1, pool.r.routed.Load())
	_, err = p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("other.test:80"))
	assert.ErrorIs(t, err, errNoExitPath)
}

func TestExitPathsListenPacket(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.onExitPath(v.path(t))
	pc, err := p.listenPacket(dialTimeout(t), M.ParseSocksaddr("example.com:53"))
	require.NoError(t, err)
	_ = pc.Close()
	conn, err := p.dial(dialTimeout(t), "udp", M.ParseSocksaddr("example.com:53"))
	require.NoError(t, err)
	_ = conn.Close()
}

// On the wire a direct exit path is never recognizably QUIC: the consumer's first datagram is
// junk, every QUIC packet after it, both ways, is Salamander-obfuscated, and no Initial
// decrypts to give up the server name. TCP and UDP still round-trip through the exit.
func TestExitPathsObfuscateTheWire(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	vConn, uConn := pathPair(t)
	rec := &recordingConn{placeholderConn: vConn}
	p.onExitPath(v.serveSession(t, rec, &trackedConn{placeholderConn: uConn}))

	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)

	pc, err := p.listenPacket(dialTimeout(t), M.ParseSocksaddr("example.com:53"))
	require.NoError(t, err)
	defer pc.Close()
	_ = pc.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = pc.WriteTo([]byte("pong"), M.ParseSocksaddr("example.com:53"))
	require.NoError(t, err)
	b := make([]byte, 64)
	n, from, err := pc.ReadFrom(b)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(b[:n]))
	assert.Equal(t, "example.com:53", from.String())
	assert.Equal(t, []string{"example.com:80", "example.com:53"}, v.r.destinations())

	first := rec.first()
	require.NotNil(t, first)
	assert.NotZero(t, first[0]&0x80, "long-header bit")
	assert.GreaterOrEqual(t, len(first), 8)
	assert.LessOrEqual(t, len(first), 64)
	assert.False(t, isQUICv1Initial(first))

	wire := rec.wire()
	require.Greater(t, len(wire), 2)
	for i, d := range wire {
		assert.False(t, isQUICv1Initial(d), "datagram %d is a QUIC v1 Initial", i)
		assert.False(t, isQUICv1LongHeader(d), "datagram %d is a QUIC v1 long-header packet", i)
	}
	// What a DPI box does with QUIC Initials: derive their keys and read the ClientHello.
	// TestDecryptQUICv1InitialReadsAStockClientsSNI shows it reads a stock client's.
	hello := initialCryptoStream(wire)
	assert.Empty(t, hello, "no datagram decrypts as a QUIC v1 Initial")
	assert.NotContains(t, string(hello), p.serverName)
}

func TestExitPathsDropAPathWithAWrongPin(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	s := v.badPinSession(t)
	p.onExitPath(s.ep)
	_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	assert.ErrorIs(t, err, errNoExitPath)
	assert.NotContains(t, err.Error(), s.ep.CertPin)
	assert.True(t, s.user.closed.Load(), "the path's conn is closed")
	assert.Equal(t, 0, p.live())
	assert.Equal(t, 1, p.failureCount())
	assert.Zero(t, v.r.routed.Load())
}

func TestExitPathsDropAnUndecodablePin(t *testing.T) {
	for name, pin := range map[string]string{"not base64": "%%%", "short": "AAAA"} {
		t.Run(name, func(t *testing.T) {
			p := newTestExitPaths(t)
			_, uConn := pathPair(t)
			u := &trackedConn{placeholderConn: uConn}
			p.onExitPath(UBClientcore.ExitPath{Conn: u, CertPin: pin, Password: "x"})
			assert.True(t, u.closed.Load())
			assert.Equal(t, 0, p.live())
		})
	}
}

func TestExitPathsBackOffAfterRepeatedFailures(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	p.now = func() time.Time { return time.Unix(0, clock.Load()) }

	failOnce := func() {
		p.onExitPath(v.badPinSession(t).ep)
		_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
		require.ErrorIs(t, err, errNoExitPath)
	}
	failOnce()
	failOnce()
	assert.True(t, p.takeExit(), "two failures")
	failOnce()
	assert.False(t, p.takeExit(), "three failures in a row")
	clock.Store(now.Add(10*time.Minute - time.Second).UnixNano())
	assert.False(t, p.takeExit())
	clock.Store(now.Add(10 * time.Minute).UnixNano())
	assert.True(t, p.takeExit(), "ten minutes on")

	failOnce()
	failOnce()
	assert.Equal(t, 2, p.failureCount())
	p.onExitPath(v.path(t))
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	assert.Zero(t, p.failureCount(), "a success reset the count")
	assert.True(t, p.takeExit())
}

// Dials that race on one path that fails count one failure: one bad volunteer must not start
// the back-off on its own.
func TestExitPathsCountAPathsFailureOnce(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.onExitPath(v.badPinSession(t).ep)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, p.failureCount())
	assert.True(t, p.takeExit())
}

func TestExitPathsKeepAPathAfterADestinationError(t *testing.T) {
	v := newVolunteerExit(t)
	v.r.refuse = "refused.example.com:443"
	p := newTestExitPaths(t)
	p.onExitPath(v.path(t))
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)

	// sing-quic sends the stream's request with its first write, so the refusal arrives on
	// the read.
	conn, err = p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr(v.r.refuse))
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = conn.Write([]byte("GET"))
		_, err = conn.Read(make([]byte, 1))
		_ = conn.Close()
	}
	assert.Error(t, err)
	assert.Equal(t, 1, p.live())
	assert.Zero(t, p.failureCount())

	conn, err = p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
}

func TestExitPathsRemoveADeadPath(t *testing.T) {
	v := newVolunteerExit(t)
	p := newTestExitPaths(t)
	p.idleTimeout = 2 * time.Second
	s := v.session(t)
	p.onExitPath(s.ep)
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)

	_ = s.volunteer.UDPConn.Close()
	require.Eventually(t, func() bool {
		_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
		return errors.Is(err, errNoExitPath)
	}, 15*time.Second, 200*time.Millisecond)
	assert.True(t, s.user.closed.Load(), "the path's conn is closed")
	assert.Equal(t, 0, p.live())
	assert.Zero(t, p.failureCount(), "a path that worked and died is no failure")
}

// A dead path beneath the newest live one is closed and dropped too, by a dial or by the next
// path's arrival, rather than held until every path above it dies.
func TestExitPathsSweepEveryDeadPath(t *testing.T) {
	for name, sweep := range map[string]func(*testing.T, *exitPaths, *volunteerExit){
		"newest":     func(_ *testing.T, p *exitPaths, _ *volunteerExit) { require.NotNil(t, p.newest()) },
		"onExitPath": func(t *testing.T, p *exitPaths, v *volunteerExit) { p.onExitPath(v.session(t).ep) },
	} {
		t.Run(name, func(t *testing.T) {
			v := newVolunteerExit(t)
			p := newTestExitPaths(t)
			older, newer := v.session(t), v.session(t)
			p.onExitPath(older.ep)
			p.onExitPath(newer.ep)
			p.mu.Lock()
			oldest := p.paths[0]
			p.mu.Unlock()
			_ = oldest.conn.Close() // its QUIC connection ended
			sweep(t, p, v)
			p.mu.Lock()
			kept := slices.Contains(p.paths, oldest)
			p.mu.Unlock()
			assert.False(t, kept, "the dead path is dropped")
			assert.True(t, older.user.closed.Load(), "and closed")
			assert.False(t, newer.user.closed.Load())
		})
	}
}

func TestExitPathsCloseClosesEveryPath(t *testing.T) {
	v := newVolunteerExit(t)
	p := newExitPaths(context.Background(), testLogger(), "www.example.com", testAllowlist(t, testAllowlistJSON))
	a, b := v.session(t), v.session(t)
	p.onExitPath(a.ep)
	p.onExitPath(b.ep)
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	p.close()
	assert.True(t, a.user.closed.Load())
	assert.True(t, b.user.closed.Load())
	assert.False(t, p.takeExit())
	_, err = p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	assert.ErrorIs(t, err, errNoExitPath)

	// A path that arrives after close is closed at once.
	c := v.session(t)
	p.onExitPath(c.ep)
	assert.True(t, c.user.closed.Load())
}

func TestExitPathsOnExitPathDoesNotBlock(t *testing.T) {
	// The volunteer never answers: onExitPath must still return at once.
	p := newTestExitPaths(t)
	_, uConn := pathPair(t)
	other := newVolunteerExit(t)
	pin, password, _ := other.e.Options().Params(false)
	done := make(chan struct{})
	go func() {
		p.onExitPath(UBClientcore.ExitPath{Conn: &trackedConn{placeholderConn: uConn}, CertPin: pin, Password: password})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onExitPath blocked")
	}
}

func TestExitPathsNeverLogSecrets(t *testing.T) {
	logs := &syncBuffer{}
	factory := sblog.NewDefaultFactory(context.Background(), sblog.Formatter{}, logs, "", nil, false)
	factory.SetLevel(sblog.LevelTrace)
	require.NoError(t, factory.Start())
	t.Cleanup(func() { _ = factory.Close() })

	v := newVolunteerExit(t)
	p := newExitPaths(context.Background(), factory.Logger(), "www.example.com", testAllowlist(t, testAllowlistJSON))
	t.Cleanup(p.close)
	// The bad path alone first, so its handshake fails rather than sitting under a newer path.
	bad := v.badPinSession(t)
	p.onExitPath(bad.ep)
	_, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.ErrorIs(t, err, errNoExitPath, "the wrong pin fails the handshake")
	good := v.session(t)
	p.onExitPath(good.ep)
	conn, err := p.dial(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	p.onExitPath(bad.ep)
	p.close()
	for _, secret := range []string{bad.ep.CertPin, good.ep.CertPin, bad.ep.Password, good.ep.Password, "192.0.2.1", "127.0.0.1"} {
		assert.NotContains(t, logs.String(), secret)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestOutboundTakesExitsOnlyWhenConfigured(t *testing.T) {
	newOut := func(opts option.UnboundedOutboundOptions) *Outbound {
		out, err := NewOutbound(context.Background(), nil, sblog.NewNOPFactory().Logger(), "unbounded-test", opts)
		require.NoError(t, err)
		o := out.(*Outbound)
		t.Cleanup(func() { _ = o.Close() })
		return o
	}
	o := newOut(option.UnboundedOutboundOptions{})
	assert.Nil(t, o.rtcOpt.OnExitPath)
	assert.Nil(t, o.rtcOpt.TakeExit)
	assert.Nil(t, o.exits)

	o = newOut(option.UnboundedOutboundOptions{DirectExit: &option.UnboundedDirectExitOptions{}})
	assert.Nil(t, o.rtcOpt.OnExitPath, "no server name")
	assert.Nil(t, o.rtcOpt.TakeExit)

	o = newOut(option.UnboundedOutboundOptions{DirectExit: &option.UnboundedDirectExitOptions{ServerName: "x"}})
	assert.NotNil(t, o.rtcOpt.OnExitPath)
	assert.NotNil(t, o.rtcOpt.TakeExit)
	require.NotNil(t, o.exits)
	assert.Nil(t, o.exits.allowlist, "no allowlist: a pool path carries nothing")

	o = newOut(option.UnboundedOutboundOptions{DirectExit: &option.UnboundedDirectExitOptions{ServerName: "x",
		PoolAllowlist: testRuleSet(t, testAllowlistJSON)}})
	require.NotNil(t, o.exits)
	assert.True(t, o.exits.allowlist.admits("tcp", M.ParseSocksaddr("example.com:443")))

	// An allowlist that isn't inline is dropped, which leaves pool paths carrying nothing,
	// rather than failing the outbound.
	o = newOut(option.UnboundedOutboundOptions{DirectExit: &option.UnboundedDirectExitOptions{ServerName: "x",
		PoolAllowlist: &sbOption.RuleSet{Type: "local", LocalOptions: sbOption.LocalRuleSet{Path: "/nonexistent"}}}})
	require.NotNil(t, o.exits)
	assert.Nil(t, o.exits.allowlist)
}

func TestOutboundDialsThroughADirectExit(t *testing.T) {
	out, err := NewOutbound(context.Background(), nil, sblog.NewNOPFactory().Logger(), "unbounded-test", option.UnboundedOutboundOptions{
		DirectExit: &option.UnboundedDirectExitOptions{ServerName: "www.example.com", PoolAllowlist: testRuleSet(t, testAllowlistJSON)},
	})
	require.NoError(t, err)
	o := out.(*Outbound)
	t.Cleanup(func() { _ = o.Close() })

	// No path, not started: today's path, which isn't up.
	_, err = o.DialContext(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	assert.ErrorContains(t, err, "not started")

	v := newVolunteerExit(t)
	s := v.session(t)
	o.rtcOpt.OnExitPath(s.ep)
	conn, err := o.DialContext(dialTimeout(t), "tcp", M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	echoes(t, conn)
	pc, err := o.ListenPacket(dialTimeout(t), M.ParseSocksaddr("example.com:53"))
	require.NoError(t, err)
	_ = pc.Close()

	// What the pool path's allowlist doesn't admit takes today's path instead of the exit.
	routed := v.r.routed.Load()
	_, err = o.DialContext(dialTimeout(t), "tcp", M.ParseSocksaddr("other.test:80"))
	assert.ErrorContains(t, err, "not started")
	_, err = o.ListenPacket(dialTimeout(t), M.ParseSocksaddr("other.test:443"))
	assert.Error(t, err)
	assert.NotErrorIs(t, err, errNoExitPath)
	assert.Equal(t, routed, v.r.routed.Load())

	require.NoError(t, o.Close())
	assert.True(t, s.user.closed.Load(), "Close closes the paths")
}

// A direct exit carries a stranger's traffic through a volunteer, so its dials' log lines name
// no destination.
func TestOutboundDirectExitLogsNoDestination(t *testing.T) {
	logs := &syncBuffer{}
	factory := sblog.NewDefaultFactory(context.Background(), sblog.Formatter{}, logs, "", nil, false)
	factory.SetLevel(sblog.LevelTrace)
	require.NoError(t, factory.Start())
	t.Cleanup(func() { _ = factory.Close() })
	out, err := NewOutbound(context.Background(), nil, factory.Logger(), "unbounded-test", option.UnboundedOutboundOptions{
		DirectExit: &option.UnboundedDirectExitOptions{ServerName: "www.example.com", PoolAllowlist: testRuleSet(t,
			`{"type": "inline", "tag": "allowlist", "rules": [{"network": ["tcp", "udp"], "domain_suffix": ["example"]}]}`)},
	})
	require.NoError(t, err)
	o := out.(*Outbound)
	t.Cleanup(func() { _ = o.Close() })
	v := newVolunteerExit(t)

	o.rtcOpt.OnExitPath(v.path(t))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = o.DialContext(canceled, "tcp", M.ParseSocksaddr("failed-dest.example:80"))
	require.Error(t, err)

	o.rtcOpt.OnExitPath(v.path(t))
	conn, err := o.DialContext(dialTimeout(t), "tcp", M.ParseSocksaddr("tcp-dest.example:80"))
	require.NoError(t, err)
	echoes(t, conn)
	pc, err := o.ListenPacket(dialTimeout(t), M.ParseSocksaddr("udp-dest.example:53"))
	require.NoError(t, err)
	_ = pc.Close()

	got := logs.String()
	assert.Contains(t, got, "unbounded dial failed")
	assert.Contains(t, got, "unbounded dial ok")
	assert.Contains(t, got, "unbounded ListenPacket ok")
	assert.NotContains(t, got, "dest.example")
}
