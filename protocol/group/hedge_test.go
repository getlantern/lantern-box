package group

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	lAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

// fakeOutbound hands out conns from server functions, so a test decides how
// each leg behaves.
type fakeOutbound struct {
	adapter.Outbound
	tag      string
	networks []string
	// stream, when set, runs the server side of each dialed conn.
	stream func(server net.Conn)
	// dialErr fails every dial.
	dialErr error
	// packet answers each datagram; nil drops it.
	packet func(p []byte) []byte
	// headroom, when set, makes packet conns require that much front
	// headroom in every buffer written to them, as SOCKS5 UDP does.
	headroom int
	// hang makes every dial wait for its context and return its error.
	hang bool
	// wrap, when set, wraps each dialed stream conn.
	wrap     func(net.Conn) net.Conn
	dialCtxs chan context.Context
	dials    atomic.Int32
	listens  atomic.Int32
}

func (f *fakeOutbound) hangUntilDone(ctx context.Context) error {
	if f.dialCtxs != nil {
		f.dialCtxs <- ctx
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second): // fail a test that never cancels, rather than hang it
		return errors.New("fake dial was never cancelled")
	}
}

func (f *fakeOutbound) Tag() string  { return f.tag }
func (f *fakeOutbound) Type() string { return "fake" }
func (f *fakeOutbound) Network() []string {
	if f.networks != nil {
		return f.networks
	}
	return []string{"tcp", "udp"}
}

func (f *fakeOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	f.dials.Add(1)
	if f.hang {
		return nil, f.hangUntilDone(ctx)
	}
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	client, server := net.Pipe()
	go f.stream(server)
	if f.wrap != nil {
		return f.wrap(client), nil
	}
	return client, nil
}

func (f *fakeOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	f.listens.Add(1)
	if f.hang {
		return nil, f.hangUntilDone(ctx)
	}
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	if f.headroom > 0 {
		return &headroomPacketConn{fakePacketConn: newFakePacketConn(f.packet), front: f.headroom}, nil
	}
	return newFakePacketConn(f.packet), nil
}

// answer replies to each read with prefix plus what it read.
func answer(prefix string) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if _, err := c.Write(append([]byte(prefix), buf[:n]...)); err != nil {
				return
			}
		}
	}
}

// blackhole reads everything and never answers, like a censor dropping the
// flow after the ClientHello.
func blackhole(c net.Conn) {
	_, _ = io.Copy(io.Discard, c)
}

// reset closes the conn after the first read, like an injected RST.
func reset(c net.Conn) {
	buf := make([]byte, 4096)
	_, _ = c.Read(buf)
	c.Close()
}

type fakePacketConn struct {
	reply  func([]byte) []byte
	in     chan []byte
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	dl     time.Time
}

func newFakePacketConn(reply func([]byte) []byte) *fakePacketConn {
	return &fakePacketConn{reply: reply, in: make(chan []byte, 64), closed: make(chan struct{})}
}

func (c *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	dl := c.dl
	c.mu.Unlock()
	var timeout <-chan time.Time
	if !dl.IsZero() {
		timer := time.NewTimer(time.Until(dl))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case b := <-c.in:
		return copy(p, b), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (c *fakePacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if c.reply != nil {
		c.in <- c.reply(append([]byte(nil), p...))
	}
	return len(p), nil
}

func (c *fakePacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *fakePacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *fakePacketConn) SetDeadline(t time.Time) error      { return c.SetReadDeadline(t) }
func (c *fakePacketConn) SetWriteDeadline(t time.Time) error { return nil }
func (c *fakePacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.dl = t
	c.mu.Unlock()
	return nil
}

// headroomPacketConn panics, as sing's SOCKS5 and Shadowsocks packet conns
// do, when a buffer lacks the front headroom it declares.
type headroomPacketConn struct {
	*fakePacketConn
	front int
}

func (c *headroomPacketConn) FrontHeadroom() int { return c.front }

func (c *headroomPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	buffer.ExtendHeader(c.front) // panics without room
	_, err := c.WriteTo(buffer.From(c.front), destination.UDPAddr())
	return err
}

func (c *headroomPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(c)
	return M.SocksaddrFromNet(addr), err
}

// dropBeforeRead closes the conn before reading, so the client's first
// write fails.
func dropBeforeRead(c net.Conn) { c.Close() }

func echoPacket(prefix string) func([]byte) []byte {
	return func(p []byte) []byte { return append([]byte(prefix), p...) }
}

type hedgeFixture struct {
	ctx   context.Context
	hedge *Hedge
	first *fakeOutbound
	then  *fakeOutbound
	mgr   *mockOutboundManager
}

func newHedgeFixture(t *testing.T, first, then *fakeOutbound, mutate func(*option.HedgeOutboundOptions)) *hedgeFixture {
	t.Helper()
	first.tag, then.tag = "first", "then"
	mgr := &mockOutboundManager{outbounds: map[string]adapter.Outbound{"first": first, "then": then}}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.OutboundManager](ctx, mgr)
	opts := option.HedgeOutboundOptions{First: "first", Then: "then", CommitTimeout: badoption.Duration(100 * time.Millisecond)}
	if mutate != nil {
		mutate(&opts)
	}
	out, err := NewHedge(ctx, nil, log.NewNOPFactory().Logger(), "hedge", opts)
	require.NoError(t, err)
	require.NoError(t, out.(*Hedge).Start())
	return &hedgeFixture{ctx: ctx, hedge: out.(*Hedge), first: first, then: then, mgr: mgr}
}

// exchange writes req and returns the reply.
func exchange(t *testing.T, c net.Conn, req string) string {
	t.Helper()
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := c.Write([]byte(req))
	require.NoError(t, err)
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	require.NoError(t, err)
	return string(buf[:n])
}

var hedgeDest = M.ParseSocksaddr("www.youtube.com:443")

// tlsRecord and quicPacket dress a payload as the opening of a TLS or QUIC
// handshake, the only connections a hedge replays.
func tlsRecord(s string) string  { return "\x16\x03\x01" + s }
func quicPacket(s string) string { return "\xc0\x00\x00\x00\x01" + s }

func (f *hedgeFixture) verdict(host string) hedgeVerdict {
	return f.hedge.cache.verdict(hedgeKeyFor(context.Background(), "tcp", M.ParseSocksaddr(host)))
}

func TestHedgeCommitsToFirst(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "first:hello", exchange(t, c, "hello"))
	require.Equal(t, "first:again", exchange(t, c, "again"))
	require.Zero(t, f.then.dials.Load())
	require.Equal(t, hedgeGood, f.verdict("www.youtube.com:443"))
}

func TestHedgeMovesWhenFirstNeverAnswers(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()

	start := time.Now()
	_, err = c.Write([]byte(tlsRecord("client-hello")))
	require.NoError(t, err)
	buf := make([]byte, 4096)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+tlsRecord("client-hello"), string(buf[:n]), "Then must receive the replayed bytes")
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, "then:more", exchange(t, c, "more"), "later writes go to Then")
	require.EqualValues(t, 1, f.then.dials.Load())

	// Another host of the same site now skips First entirely.
	require.Equal(t, hedgeBad, f.verdict("rr3---sn-abc.youtube.com:443"))
	c2, err := f.hedge.DialContext(f.ctx, "tcp", M.ParseSocksaddr("rr3---sn-abc.youtube.com:443"))
	require.NoError(t, err)
	defer c2.Close()
	require.Equal(t, "then:x", exchange(t, c2, "x"))
	require.EqualValues(t, 1, f.first.dials.Load(), "a site known bad must not try First")
}

func TestHedgeMovesOnResetWithoutWaiting(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: reset}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	start := time.Now()
	require.Equal(t, "then:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
	require.Less(t, time.Since(start), 2*time.Second, "a reset must move at once, not after the commit timeout")
	require.Equal(t, hedgeBad, f.verdict("www.youtube.com:443"))
}

func TestHedgeMovesOnDialError(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{dialErr: errors.New("no strategy")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "then:hi", exchange(t, c, "hi"))
	require.Equal(t, hedgeBad, f.verdict("www.youtube.com:443"))
}

func TestHedgeProbesBypassFirst(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	probe := lAdapter.ContextWithProbe(f.ctx)
	c, err := f.hedge.DialContext(probe, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "then:probe", exchange(t, c, "probe"))
	pc, err := f.hedge.ListenPacket(probe, hedgeDest)
	require.NoError(t, err)
	pc.Close()
	require.Zero(t, f.first.dials.Load()+f.first.listens.Load(), "a probe must never touch First")
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "a probe must not teach the cache")
}

func TestHedgeProbesBypassNestedHedges(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	outer := &fakeOutbound{stream: answer("outer-first:")}
	f.mgr.outbounds["outer-first"] = outer
	f.mgr.outbounds["inner"] = f.hedge
	o, err := NewHedge(f.ctx, nil, log.NewNOPFactory().Logger(), "outer",
		option.HedgeOutboundOptions{First: "outer-first", Then: "inner"})
	require.NoError(t, err)
	c, err := o.DialContext(lAdapter.ContextWithProbe(f.ctx), "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "then:probe", exchange(t, c, "probe"))
	require.Zero(t, outer.dials.Load())
	require.Zero(t, f.first.dials.Load())
}

func TestHedgeSkipsFirstWithoutTheNetwork(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{networks: []string{"tcp"}, packet: echoPacket("first:")},
		&fakeOutbound{packet: echoPacket("then:")}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	require.Zero(t, f.first.listens.Load())
	require.EqualValues(t, 1, f.then.listens.Load())
}

func TestHedgeOverflowStaysOnFirst(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.MaxHeldBytes = 4 })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte(tlsRecord("more than four bytes")))
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Read(make([]byte, 64))
	require.Error(t, err, "an unreplayable connection fails so the client retries")
	require.Zero(t, f.then.dials.Load(), "bytes that weren't kept can't be replayed to Then")
	require.Equal(t, hedgeBad, f.verdict("www.youtube.com:443"), "the retry must go to Then")
}

func TestHedgeFailureAfterCommitOnlyMarksBad(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "first:hi", exchange(t, c, "hi"))
	s := c.(*hedgeStream)
	s.firstFailed(lAdapter.UserFailureStall, s.move)
	require.Equal(t, hedgeBad, f.verdict("www.youtube.com:443"))
	require.Zero(t, f.then.dials.Load(), "the client saw First's bytes, so the conn can't move")
}

func TestHedgeUDPMovesAndReplays(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{}, &fakeOutbound{packet: echoPacket("then:")}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	dest := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	for _, p := range []string{quicPacket("initial-1"), quicPacket("initial-2")} {
		_, err = pc.WriteTo([]byte(p), dest)
		require.NoError(t, err)
	}
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	for _, want := range []string{"then:" + quicPacket("initial-1"), "then:" + quicPacket("initial-2")} {
		n, _, err := pc.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, want, string(buf[:n]))
	}
	require.EqualValues(t, 1, f.then.listens.Load())
	require.Equal(t, hedgeBad, f.hedge.cache.verdict(hedgeKeyFor(context.Background(), "udp", hedgeDest)))
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "UDP and TCP results are kept apart")
}

func TestHedgeUDPCommitsToFirst(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{packet: echoPacket("first:")}, &fakeOutbound{packet: echoPacket("then:")}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	_, err = pc.WriteTo([]byte("initial"), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
	require.NoError(t, err)
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, _, err := pc.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "first:initial", string(buf[:n]))
	time.Sleep(200 * time.Millisecond) // past the commit timeout
	require.Zero(t, f.then.listens.Load())
	require.Equal(t, hedgeGood, f.hedge.cache.verdict(hedgeKeyFor(context.Background(), "udp", hedgeDest)))
}

func TestHedgeInterfaceUpdatedForgets(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{dialErr: errors.New("blocked")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	c.Close()
	require.Equal(t, hedgeBad, f.verdict("www.youtube.com:443"))
	f.hedge.InterfaceUpdated(context.Background())
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"))
}

func TestHedgeCacheSharedAcrossHedges(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{dialErr: errors.New("blocked")}, &fakeOutbound{stream: answer("then:")}, nil)
	other, err := NewHedge(f.ctx, nil, log.NewNOPFactory().Logger(), "other", option.HedgeOutboundOptions{First: "first", Then: "then"})
	require.NoError(t, err)
	require.Same(t, f.hedge.cache, other.(*Hedge).cache)
}

func TestNewHedgeRejectsBadTags(t *testing.T) {
	ctx := service.ContextWithDefaultRegistry(context.Background())
	for _, opts := range []option.HedgeOutboundOptions{{First: "a"}, {Then: "b"}, {First: "a", Then: "a"}} {
		_, err := NewHedge(ctx, nil, log.NewNOPFactory().Logger(), "h", opts)
		require.Error(t, err, "%+v", opts)
	}
}

func TestHedgeKeyFor(t *testing.T) {
	ip := netip.MustParseAddr("142.250.1.7")
	sniffed := adapter.WithContext(context.Background(), &adapter.InboundContext{
		Domain: "rr4---sn-q4f.googlevideo.com", Destination: M.Socksaddr{Addr: ip, Port: 443}})
	other := adapter.WithContext(context.Background(), &adapter.InboundContext{
		Domain: "unrelated.example", Destination: M.ParseSocksaddr("192.0.2.9:443")})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		dest M.Socksaddr
		site string
	}{
		{"hostname", context.Background(), M.ParseSocksaddr("RR1---sn-x.GoogleVideo.com.:443"), "googlevideo.com"},
		{"multi-part suffix", context.Background(), M.ParseSocksaddr("news.bbc.co.uk:443"), "bbc.co.uk"},
		{"sniffed name for this address", sniffed, M.Socksaddr{Addr: ip, Port: 443}, "googlevideo.com"},
		{"another connection's name is ignored", other, M.Socksaddr{Addr: ip, Port: 443}, "ip/142.250.1.0/24"},
		{"IPv6 without a name", context.Background(), M.ParseSocksaddr("[2001:db8:1:2::5]:443"), "ip/2001:db8:1::/48"},
		{"no host", context.Background(), M.Socksaddr{Port: 443}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.site, hedgeKeyFor(tc.ctx, "tcp", tc.dest).site)
		})
	}
}

func TestHedgeCacheAddressBlockPrior(t *testing.T) {
	c := newHedgeCache()
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	block := func(site string) hedgeKey {
		return hedgeKey{site: site, network: "udp", prefix: netip.MustParsePrefix("142.250.1.0/24")}
	}

	c.mark(block("googlevideo.com"), hedgeBad, time.Minute)
	require.Equal(t, hedgeUnknown, c.verdict(block("ytimg.com")), "one bad site is not enough to judge a block")
	c.mark(block("youtube.com"), hedgeBad, time.Minute)
	require.Equal(t, hedgeBad, c.verdict(block("ggpht.com")), "two bad sites mark the block")
	require.Equal(t, hedgeUnknown, c.verdict(hedgeKey{site: "ggpht.com", network: "tcp",
		prefix: netip.MustParsePrefix("142.250.1.0/24")}), "the prior is per network")

	c.mark(block("ggpht.com"), hedgeGood, time.Hour)
	require.Equal(t, hedgeGood, c.verdict(block("ggpht.com")), "a site's own result beats the block's")

	now = now.Add(2 * time.Minute)
	require.Equal(t, hedgeUnknown, c.verdict(block("youtube.com")), "failures expire")
	require.Equal(t, hedgeUnknown, c.verdict(block("other.com")), "expired failures stop marking the block")
	require.Equal(t, hedgeUnknown, c.verdict(hedgeKey{network: "udp"}), "no site, no verdict")
}

// TestHedgeReplyRacingMoveIsDiscarded covers a reply from First that lands
// after a move began: the client must not see it, since the replay to Then
// will draw its own, and First must not be marked good for it.
func TestHedgeReplyRacingMoveIsDiscarded(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	s := c.(*hedgeStream)
	s.mu.Lock()
	s.moving = true
	s.mu.Unlock()
	require.False(t, s.commit(s.releaseLocked), "a reply racing a move must be discarded")
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"))
}

func TestHedgeKeepsPlaintextOnFirst(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte("POST /pay HTTP/1.1\r\n\r\n"))
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(400*time.Millisecond)))
	_, err = c.Read(make([]byte, 64))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "a slow plaintext reply is left to the client's own timeout")
	require.Zero(t, f.then.dials.Load(), "plaintext must never be replayed to Then")
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "a slow reply is not a block")
}

func TestHedgePlaintextResetMarksBadWithoutMoving(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: reset}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	// net.Pipe refuses deadlines once its peer closed, so none is set; the
	// peer's close ends the Read.
	_, err = c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	require.NoError(t, err)
	_, err = c.Read(make([]byte, 64))
	require.Error(t, err)
	require.Zero(t, f.then.dials.Load())
	require.Eventually(t, func() bool { return f.verdict("www.youtube.com:443") == hedgeBad },
		time.Second, 10*time.Millisecond, "the client's retry must go to Then")
}

func TestHedgeMovesWhenWriteToFirstFails(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: dropBeforeRead}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	time.Sleep(20 * time.Millisecond) // let First's peer close
	n, err := c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err, "a failed write moves the connection; the replay delivers it")
	require.Equal(t, len(tlsRecord("hello")), n)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, err = c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+tlsRecord("hello"), string(buf[:n]))
	require.EqualValues(t, 1, f.then.dials.Load())
}

func TestHedgeReturnsThenDialError(t *testing.T) {
	errThen := errors.New("then unreachable")
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{dialErr: errThen}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Read(make([]byte, 64))
	require.ErrorIs(t, err, errThen)
	_, err = c.Write([]byte("more"))
	require.ErrorIs(t, err, errThen)
}

// startMoveToHangingThen starts a move whose dial to Then hangs, and returns
// the hedged conn and the dial's context.
func startMoveToHangingThen(t *testing.T) (*hedgeFixture, net.Conn, context.Context) {
	t.Helper()
	then := &fakeOutbound{hang: true, dialCtxs: make(chan context.Context, 1)}
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, then, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	select {
	case ctx := <-then.dialCtxs:
		return f, c, ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the move never dialed Then")
	}
	return nil, nil, nil
}

func TestHedgeCloseDuringMoveCancelsThenDial(t *testing.T) {
	_, c, dialCtx := startMoveToHangingThen(t)
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a move in progress")
	}
	select {
	case <-dialCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the dial to Then")
	}
	_, err := c.Write([]byte("x"))
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestHedgeReadDeadlineBoundsWaitForMove(t *testing.T) {
	_, c, _ := startMoveToHangingThen(t)
	defer c.Close()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	start := time.Now()
	_, err := c.Read(make([]byte, 64))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second, "a Read must not outlive its deadline waiting for Then")
	require.NoError(t, c.SetWriteDeadline(time.Now().Add(100*time.Millisecond)))
	_, err = c.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestHedgeReappliesReadDeadlineToThen(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: blackhole}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(600*time.Millisecond)))
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	start := time.Now()
	_, err = c.Read(make([]byte, 64))
	require.True(t, isTimeout(err), "got %v", err)
	require.Less(t, time.Since(start), 3*time.Second, "Then's leg must carry the caller's read deadline")
	require.EqualValues(t, 1, f.then.dials.Load())
}

func TestHedgeReleasesCopyAtCommit(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "first:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
	s := c.(*hedgeStream)
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Nil(t, s.held, "a committed connection must not keep its copy")
}

func TestHedgeUDPReplayGivesThenHeadroom(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{}, &fakeOutbound{packet: echoPacket("then:"), headroom: 10}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	dest := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	_, err = pc.WriteTo([]byte(quicPacket("initial")), dest)
	require.NoError(t, err)
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, _, err := pc.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+quicPacket("initial"), string(buf[:n]))
	_, err = pc.WriteTo([]byte("after"), dest)
	require.NoError(t, err, "writes after the move need headroom too")
}

func TestHedgeSlowFirstDialFallsBackWithoutMarkingBad(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{hang: true}, &fakeOutbound{stream: answer("then:")}, nil)
	start := time.Now()
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Less(t, time.Since(start), 2*time.Second, "First's dial is bounded by the commit timeout")
	require.Equal(t, "then:hi", exchange(t, c, "hi"))
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "a slow dial is not a block")
}

func TestHedgeCacheBoundsAddressBlockIndex(t *testing.T) {
	c := newHedgeCache()
	for i := range hedgeMaxEntries + 500 {
		addr := netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})
		prefix, _ := addr.Prefix(24)
		c.mark(hedgeKey{site: "flaky.example", network: "tcp", prefix: prefix}, hedgeBad, time.Hour)
	}
	records := 0
	for _, sites := range c.badSites {
		records += len(sites)
	}
	require.LessOrEqual(t, records, hedgeMaxEntries, "one site failing across many blocks must stay bounded")
}

func TestHedgeSwitch(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.Disabled = true })
	var sw lAdapter.Switchable = f.hedge
	require.False(t, sw.Enabled(), "disabled starts the hedge off")
	dial := func() string {
		c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
		require.NoError(t, err)
		defer c.Close()
		return exchange(t, c, tlsRecord("hi"))
	}

	require.Equal(t, "then:"+tlsRecord("hi"), dial())
	require.Zero(t, f.first.dials.Load(), "a hedge switched off must not try First")
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "nor touch the cache")

	sw.SetEnabled(true)
	require.Equal(t, "first:"+tlsRecord("hi"), dial())
	require.EqualValues(t, 1, f.first.dials.Load())

	sw.SetEnabled(false)
	require.Equal(t, "then:"+tlsRecord("hi"), dial())
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	pc.Close()
	require.EqualValues(t, 1, f.first.dials.Load())
	require.Zero(t, f.first.listens.Load(), "switching off covers UDP too")
}

// doneCtx is a context whose Done channel the context package can't see
// into, so a child that is never cancelled costs a goroutine for as long as
// the parent lives.
type doneCtx struct {
	context.Context
	done chan struct{}
}

func (c *doneCtx) Done() <-chan struct{} { return c.done }

func (c *doneCtx) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func TestHedgeFirstDialLeavesNothingOnTheParent(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	parent := &doneCtx{Context: f.ctx, done: make(chan struct{})}
	defer close(parent.done)
	before := runtime.NumGoroutine()
	for range 50 {
		c, err := f.hedge.DialContext(parent, "tcp", hedgeDest)
		require.NoError(t, err)
		require.Equal(t, "first:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
		c.Close()
	}
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+5 },
		2*time.Second, 20*time.Millisecond, "each successful dial left something waiting on the parent context")
}

func TestHedgeClearedWriteDeadlineDuringReplay(t *testing.T) {
	release := make(chan struct{})
	slowThen := func(c net.Conn) {
		<-release // hold the replay until the test has cleared the deadline
		answer("then:")(c)
	}
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: slowThen}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.SetWriteDeadline(time.Now().Add(400*time.Millisecond)))
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	s := c.(*hedgeStream)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending != nil
	}, 2*time.Second, 5*time.Millisecond, "the replay never started")
	require.NoError(t, c.SetWriteDeadline(time.Time{}))
	close(release)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+tlsRecord("hello"), string(buf[:n]))
	time.Sleep(500 * time.Millisecond) // past the deadline that was cleared
	_, err = c.Write([]byte("later"))
	require.NoError(t, err, "a deadline cleared during the replay must not survive on Then's leg")
}

// earlyConn waits for a first write before it can be read from, like the
// client-info frame conn or a deferred-handshake proxy.
type earlyConn struct {
	net.Conn
	wrote atomic.Bool
}

func (c *earlyConn) NeedHandshakeForWrite() bool { return !c.wrote.Load() }
func (c *earlyConn) Write(p []byte) (int, error) {
	c.wrote.Store(true)
	if len(p) == 0 {
		return 0, nil
	}
	return c.Conn.Write(p)
}

func TestHedgeForwardsDeferredHandshake(t *testing.T) {
	var early *earlyConn
	first := &fakeOutbound{stream: blackhole, wrap: func(c net.Conn) net.Conn { early = &earlyConn{Conn: c}; return early }}
	f := newHedgeFixture(t, first, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.True(t, N.NeedHandshakeForWrite(c), "the router must see First's pending handshake")
	_, err = c.Write(nil) // the router's kick for a server-first protocol
	require.NoError(t, err)
	require.False(t, N.NeedHandshakeForWrite(c))
	require.True(t, early.wrote.Load(), "the kick must reach First")
	// The kick must not decide replayability: a handshake written next still moves.
	require.Equal(t, "then:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
}

// halfCloser records CloseWrite.
type halfCloser struct {
	net.Conn
	closedWrite atomic.Bool
}

func (c *halfCloser) CloseWrite() error { c.closedWrite.Store(true); return nil }

func TestHedgeHalfCloseOnFirst(t *testing.T) {
	var hc *halfCloser
	first := &fakeOutbound{stream: answer("first:"), wrap: func(c net.Conn) net.Conn { hc = &halfCloser{Conn: c}; return hc }}
	f := newHedgeFixture(t, first, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "first:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
	wc, ok := c.(N.WriteCloser)
	require.True(t, ok)
	require.NoError(t, wc.CloseWrite())
	require.True(t, hc.closedWrite.Load())
}

func TestHedgeHalfCloseCarriesAcrossMove(t *testing.T) {
	var thenConn *halfCloser
	then := &fakeOutbound{stream: answer("then:"), wrap: func(c net.Conn) net.Conn { thenConn = &halfCloser{Conn: c}; return thenConn }}
	first := &fakeOutbound{stream: blackhole, wrap: func(c net.Conn) net.Conn { return &halfCloser{Conn: c} }}
	f := newHedgeFixture(t, first, then, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte(tlsRecord("request")))
	require.NoError(t, err)
	require.NoError(t, c.(N.WriteCloser).CloseWrite(), "half-close while still on First")
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+tlsRecord("request"), string(buf[:n]))
	require.True(t, thenConn.closedWrite.Load(), "the half-close must be repeated on Then after the replay")
}

func TestHedgeCloseWriteUnsupported(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.ErrorIs(t, c.(N.WriteCloser).CloseWrite(), errors.ErrUnsupported,
		"a leg that can't half-close must say so, so the relay closes instead")
}

// noDeadlineConn ignores deadlines, as samizdat-reverse's pipe does.
type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetDeadline(time.Time) error      { return os.ErrInvalid }
func (noDeadlineConn) SetReadDeadline(time.Time) error  { return os.ErrInvalid }
func (noDeadlineConn) SetWriteDeadline(time.Time) error { return os.ErrInvalid }

func TestHedgeReplayBoundedWithoutDeadlines(t *testing.T) {
	old := hedgeFailoverDialTimeout
	hedgeFailoverDialTimeout = 300 * time.Millisecond
	t.Cleanup(func() { hedgeFailoverDialTimeout = old })
	stuck := func(c net.Conn) { time.Sleep(10 * time.Second); c.Close() } // never reads the replay
	then := &fakeOutbound{stream: stuck, wrap: func(c net.Conn) net.Conn { return noDeadlineConn{c} }}
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, then, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	start := time.Now()
	_, err = c.Read(make([]byte, 64))
	require.Error(t, err)
	require.Less(t, time.Since(start), 3*time.Second, "a replay to a conn without deadlines must still end")
}

func TestHedgeWriteErrorMoveHonorsDeadline(t *testing.T) {
	dropNow := make(chan struct{})
	first := &fakeOutbound{stream: func(c net.Conn) { <-dropNow; c.Close() }}
	f := newHedgeFixture(t, first, &fakeOutbound{hang: true},
		func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	// Set before First's peer closes: net.Pipe refuses deadlines after that.
	require.NoError(t, c.SetWriteDeadline(time.Now().Add(150*time.Millisecond)))
	close(dropNow)
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second, "the write that triggers a move is bound by its deadline")
}

func TestHedgeWaitersSeeDeadlineChanges(t *testing.T) {
	_, c, _ := startMoveToHangingThen(t)
	defer c.Close()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(150*time.Millisecond)))
	done := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 64))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, c.SetReadDeadline(time.Time{}), "clearing the deadline must reach a waiting Read")
	select {
	case err := <-done:
		t.Fatalf("Read returned %v at the old deadline", err)
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, c.SetReadDeadline(time.Now().Add(50*time.Millisecond)))
	select {
	case err := <-done:
		require.ErrorIs(t, err, os.ErrDeadlineExceeded, "shortening it must too")
	case <-time.After(2 * time.Second):
		t.Fatal("Read missed the new deadline")
	}
}

func TestHedgeDeadlineReachesPendingReplay(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slowThen := func(c net.Conn) {
		<-release
		c.Close()
	}
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: slowThen}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte(tlsRecord("hello")))
	require.NoError(t, err)
	s := c.(*hedgeStream)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending != nil
	}, 2*time.Second, 5*time.Millisecond, "the replay never started")
	require.NoError(t, c.SetWriteDeadline(time.Now().Add(100*time.Millisecond)))
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	start := time.Now()
	_, err = c.Read(make([]byte, 64))
	require.Error(t, err, "a deadline set during the replay must interrupt it")
	require.Less(t, time.Since(start), 2*time.Second)
}

// quicInitial builds a long-header packet with the given connection IDs.
func quicInitial(dcid, scid []byte, payload string) []byte {
	b := []byte{0xc0, 0, 0, 0, 1, byte(len(dcid))}
	b = append(b, dcid...)
	b = append(b, byte(len(scid)))
	b = append(b, scid...)
	return append(b, payload...)
}

// versionNegotiation builds a Version Negotiation packet addressed to dcid.
func versionNegotiation(dcid []byte) []byte {
	b := []byte{0x80, 0, 0, 0, 0, byte(len(dcid))}
	b = append(b, dcid...)
	return append(b, 0, 0, 0, 1)
}

func TestHedgeDecoyVersionNegotiationDoesNotCommit(t *testing.T) {
	decoyCID := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	first := &fakeOutbound{packet: func([]byte) []byte { return versionNegotiation(decoyCID) }}
	f := newHedgeFixture(t, first, &fakeOutbound{packet: echoPacket("then:")}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	initial := quicInitial([]byte{1, 2, 3, 4, 5, 6, 7, 8}, []byte{7, 7, 7, 7}, "hello")
	_, err = pc.WriteTo(initial, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
	require.NoError(t, err)
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 256)
	n, _, err := pc.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "then:"+string(initial), string(buf[:n]),
		"a Version Negotiation answering only a decoy must not keep the flow on First")
	require.EqualValues(t, 1, f.then.listens.Load())
}

func TestHedgeOwnVersionNegotiationIsDelivered(t *testing.T) {
	scid := []byte{7, 7, 7, 7}
	first := &fakeOutbound{packet: func([]byte) []byte { return versionNegotiation(scid) }}
	f := newHedgeFixture(t, first, &fakeOutbound{packet: echoPacket("then:")}, nil)
	pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	_, err = pc.WriteTo(quicInitial([]byte{1, 2, 3, 4}, scid, "hello"), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
	require.NoError(t, err)
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 256)
	n, _, err := pc.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, string(versionNegotiation(scid)), string(buf[:n]), "the client's own Version Negotiation reaches it")
	require.Zero(t, f.then.listens.Load())
}

func TestHedgeCacheDropsResultsFromBeforeANetworkChange(t *testing.T) {
	c := newHedgeCache()
	key := hedgeKey{site: "youtube.com", network: "tcp", gen: c.generation()}
	c.clear()
	c.mark(key, hedgeBad, time.Hour)
	key.gen = c.generation()
	require.Equal(t, hedgeUnknown, c.verdict(key), "a result learned on the old network must be dropped")
	c.mark(key, hedgeBad, time.Hour)
	require.Equal(t, hedgeBad, c.verdict(key))
}

func TestHedgeConnFromBeforeANetworkChangeDoesNotMark(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	f.hedge.InterfaceUpdated(context.Background())
	require.Equal(t, "then:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))
	require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"),
		"First failing for a connection made on the old network says nothing about the new one")
}

func TestHedgeCacheDropsEmptyAddressBlocks(t *testing.T) {
	c := newHedgeCache()
	for i := range 100 {
		prefix, _ := netip.AddrFrom4([4]byte{10, 0, byte(i), 1}).Prefix(24)
		k := hedgeKey{site: "flaky.example", network: "tcp", prefix: prefix}
		c.mark(k, hedgeBad, time.Hour)
		c.mark(k, hedgeGood, time.Hour)
	}
	require.Empty(t, c.badSites, "a block whose last bad site recovered must be removed")
}

// udpOutbound returns real UDP sockets, which accept only *net.UDPAddr.
type udpOutbound struct {
	adapter.Outbound
	tag string
}

func (u *udpOutbound) Tag() string       { return u.tag }
func (u *udpOutbound) Type() string      { return "udp" }
func (u *udpOutbound) Network() []string { return []string{"udp"} }
func (u *udpOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return net.ListenUDP("udp4", nil)
}

func TestHedgeDialUDPBypassingFirstToPlainSocket(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], from)
		}
	}()
	f := newHedgeFixture(t, &fakeOutbound{}, &fakeOutbound{}, func(o *option.HedgeOutboundOptions) { o.Disabled = true })
	f.mgr.outbounds["then"] = &udpOutbound{tag: "then"}
	c, err := f.hedge.DialContext(f.ctx, "udp", M.SocksaddrFromNet(echo.LocalAddr()))
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err, "a plain UDP socket must get a *net.UDPAddr")
	require.NoError(t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
}
