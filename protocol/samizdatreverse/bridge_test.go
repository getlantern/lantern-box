package samizdatreverse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/tracker/peerconn"
	samizdat "github.com/getlantern/samizdat"
)

// dialRouter routes each connection by dialing its destination directly and
// relaying, as a direct outbound would.
type dialRouter struct {
	mu     sync.Mutex
	routed []adapter.InboundContext
	closed atomic.Int32 // routed connections that have finished
}

func (r *dialRouter) RouteConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *dialRouter) RoutePacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *dialRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.mu.Lock()
	r.routed = append(r.routed, metadata)
	r.mu.Unlock()
	go func() {
		up, err := net.Dial("tcp", metadata.Destination.String())
		if err != nil {
			conn.Close()
			onClose(err)
			return
		}
		err = bufio.CopyConn(ctx, conn, up)
		conn.Close()
		up.Close()
		onClose(nil)
		r.closed.Add(1)
		_ = err
	}()
}

func (r *dialRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	conn.Close()
	onClose(errors.New("not used"))
}

func (r *dialRouter) lastRouted() (adapter.InboundContext, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routed) == 0 {
		return adapter.InboundContext{}, false
	}
	return r.routed[len(r.routed)-1], true
}

func certPin(t *testing.T, k testKeys) string {
	t.Helper()
	block, _ := pem.Decode(k.cert)
	require.NotNil(t, block)
	pin, err := samizdat.SPKISHA256(block.Bytes)
	require.NoError(t, err)
	return hex.EncodeToString(pin[:])
}

func bridgeOptions(t *testing.T, addr string, k testKeys, id [8]byte) option.SamizdatBridgeInboundOptions {
	t.Helper()
	return option.SamizdatBridgeInboundOptions{
		Addrs:          []string{addr},
		PublicKey:      hex.EncodeToString(k.pub),
		ShortID:        hex.EncodeToString(id[:]),
		ServerName:     "localhost",
		CertSPKISHA256: certPin(t, k),
	}
}

// newTestBridge creates a bridge that dials with a plain net.Dialer and
// routes through router, and starts it.
func newTestBridge(t *testing.T, opts option.SamizdatBridgeInboundOptions, router adapter.ConnectionRouterEx) *Bridge {
	t.Helper()
	in, err := NewBridge(context.Background(), nil, sblog.NewNOPFactory().Logger(), "bridge", opts)
	require.NoError(t, err)
	b := in.(*Bridge)
	b.dial = (&net.Dialer{}).DialContext
	if router != nil {
		b.router = router
	}
	require.NoError(t, b.Start(adapter.StartStateStart))
	t.Cleanup(func() { b.Close() })
	return b
}

func TestBridgeServesReverseDials(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))

	var events []peerconn.Event
	var evMu sync.Mutex
	peerconn.SetListener(func(evt peerconn.Event) {
		evMu.Lock()
		events = append(events, evt)
		evMu.Unlock()
	})
	t.Cleanup(func() { peerconn.SetListener(nil) })

	router := &dialRouter{}
	newTestBridge(t, bridgeOptions(t, portalAddr(t, p), k, id), router)
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	o := newTestReverse(t, tagA, p)
	echo := tcpEcho(t)
	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(echo))
	require.NoError(t, err)
	defer conn.Close()

	for _, msg := range []string{"hello", "again"} {
		_, err = conn.Write([]byte(msg))
		require.NoError(t, err)
		buf := make([]byte, len(msg))
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		assert.Equal(t, msg, string(buf))
	}

	// Half-close from the inviter reaches the echo, which ends the stream.
	require.NoError(t, N.CloseWrite(conn))
	n, err := conn.Read(make([]byte, 1))
	assert.Zero(t, n)
	assert.ErrorIs(t, err, io.EOF)

	md, ok := router.lastRouted()
	require.True(t, ok)
	assert.Equal(t, "bridge", md.Inbound)
	assert.Equal(t, echo, md.Destination.String())

	// The bridge reports nothing to peerconn: radiance counts bridge bytes
	// itself, and peer share's listener in the same process must not count
	// them as pool traffic. The listener is live, as Acquire shows. Once the
	// routed connection has finished, the bridge's handler returns at once,
	// so a close event would follow within moments.
	require.NotNil(t, peerconn.Acquire(), "test listener is not registered")
	require.Eventually(t, func() bool { return router.closed.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	evMu.Lock()
	defer evMu.Unlock()
	assert.Empty(t, events, "the bridge emitted peerconn events")
}

func TestBridgeRejectsNonConnect(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	router := &dialRouter{}
	newTestBridge(t, bridgeOptions(t, portalAddr(t, p), k, id), router)
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	cc, _ := p.Session(tagA)
	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	require.NoError(t, err)
	resp, err := cc.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 405, resp.StatusCode)
	_, routed := router.lastRouted()
	assert.False(t, routed)
}

func TestBridgeWrongPinNeverEstablishes(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))

	opts := bridgeOptions(t, portalAddr(t, p), k, id)
	wrong := make([]byte, 32)
	_, err := rand.Read(wrong)
	require.NoError(t, err)
	opts.CertSPKISHA256 = hex.EncodeToString(wrong)
	newTestBridge(t, opts, &dialRouter{})

	assert.Never(t, func() bool { _, ok := p.Session(tagA); return ok }, 2*time.Second, 50*time.Millisecond)
}

func TestBridgeReconnectsAfterPortalRestart(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	port := freePort(t)
	require.NoError(t, p.Configure(k.config(port, map[[8]byte]string{id: tagA})))
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	newTestBridge(t, bridgeOptions(t, addr, k, id), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	// Move away, which ends the session, then back.
	require.NoError(t, p.Configure(k.config(freePort(t), map[[8]byte]string{id: tagA})))
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return !ok }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, p.Configure(k.config(port, map[[8]byte]string{id: tagA})))

	// Two short sessions and at most a couple of refused dials put the
	// backoff at a few seconds at most.
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 10*time.Second, 50*time.Millisecond)
}

func TestBridgeCloseEndsSession(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	b := newTestBridge(t, bridgeOptions(t, portalAddr(t, p), k, id), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return !ok }, 5*time.Second, 20*time.Millisecond)
}

func TestBridgeBackoff(t *testing.T) {
	pinErr := fmt.Errorf("handshake: %w", samizdat.ErrCertPinMismatch)
	refused := errors.New("refused")
	// Each step is one attempt's outcome: how long its session lasted (zero
	// when none was established) and the error that ended it.
	type step struct {
		lasted time.Duration
		err    error
	}
	steps := []step{
		{0, refused},            // 1s
		{0, pinErr},             // 2s: a pin mismatch backs off the same way
		{0, refused},            // 4s
		{30 * time.Second, nil}, // 8s: a short session does not reset
		{2 * time.Minute, nil},  // 1s: a long one does
		{0, refused},            // 2s
		{0, refused},            // 4s
		{0, refused},            // 8s
		{0, refused},            // 16s
		{0, refused},            // 32s
		{0, refused},            // 64s
		{0, refused},            // 128s
		{0, refused},            // 256s
		{0, refused},            // 300s: capped
		{0, refused},            // 300s
	}
	want := []time.Duration{1, 2, 4, 8, 1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}

	in, err := NewBridge(context.Background(), nil, sblog.NewNOPFactory().Logger(), "bridge", bridgeOptions(t, "127.0.0.1:1", newTestKeys(t), newShortID(t)))
	require.NoError(t, err)
	b := in.(*Bridge)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var i int
	b.attempt = func(context.Context) (time.Duration, error) {
		s := steps[i]
		i++
		return s.lasted, s.err
	}
	b.jitter = func(d time.Duration) time.Duration { return d }
	var got []time.Duration
	b.sleep = func(ctx context.Context, d time.Duration) bool {
		got = append(got, d/time.Second)
		if len(got) == len(want) {
			cancel()
			return false
		}
		return true
	}
	b.run(ctx)
	assert.Equal(t, want, got)
}

func TestBridgeFullJitter(t *testing.T) {
	for range 1000 {
		d := fullJitter(4 * time.Second)
		require.GreaterOrEqual(t, d, time.Duration(0))
		require.LessOrEqual(t, d, 4*time.Second)
	}
}

func TestBridgeOptionValidation(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	good := bridgeOptions(t, "127.0.0.1:443", k, id)
	cases := map[string]func(*option.SamizdatBridgeInboundOptions){
		"no addrs": func(o *option.SamizdatBridgeInboundOptions) { o.Addrs = nil },
		"three addrs": func(o *option.SamizdatBridgeInboundOptions) {
			o.Addrs = []string{"192.0.2.1:1", "[2001:db8::1]:1", "192.0.2.2:1"}
		},
		"two IPv4":       func(o *option.SamizdatBridgeInboundOptions) { o.Addrs = []string{"192.0.2.1:1", "192.0.2.2:1"} },
		"two IPv6":       func(o *option.SamizdatBridgeInboundOptions) { o.Addrs = []string{"[2001:db8::1]:1", "[2001:db8::2]:1"} },
		"hostname":       func(o *option.SamizdatBridgeInboundOptions) { o.Addrs = []string{"portal.example.com:443"} },
		"short key":      func(o *option.SamizdatBridgeInboundOptions) { o.PublicKey = "abcd" },
		"bad short ID":   func(o *option.SamizdatBridgeInboundOptions) { o.ShortID = "zz" },
		"short pin":      func(o *option.SamizdatBridgeInboundOptions) { o.CertSPKISHA256 = "00" },
		"no pin":         func(o *option.SamizdatBridgeInboundOptions) { o.CertSPKISHA256 = "" },
		"no server name": func(o *option.SamizdatBridgeInboundOptions) { o.ServerName = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := good
			opts.Addrs = append([]string(nil), good.Addrs...)
			mutate(&opts)
			_, err := NewBridge(context.Background(), nil, sblog.NewNOPFactory().Logger(), "bridge", opts)
			require.Error(t, err)
			for _, secret := range append(opts.Addrs, opts.PublicKey, opts.ShortID, opts.CertSPKISHA256) {
				if len(secret) > 2 {
					assert.NotContains(t, err.Error(), secret)
				}
			}
		})
	}

	both := good
	both.Addrs = []string{"[2001:db8::1]:443", "192.0.2.1:443"}
	_, err := NewBridge(context.Background(), nil, sblog.NewNOPFactory().Logger(), "bridge", both)
	assert.NoError(t, err)
}

func TestBridgeOrdersIPv6First(t *testing.T) {
	opts := bridgeOptions(t, "192.0.2.1:443", newTestKeys(t), newShortID(t))
	opts.Addrs = []string{"192.0.2.1:443", "[2001:db8::1]:443"}
	in, err := NewBridge(context.Background(), nil, sblog.NewNOPFactory().Logger(), "bridge", opts)
	require.NoError(t, err)
	b := in.(*Bridge)
	require.Len(t, b.addrs, 2)
	assert.True(t, b.addrs[0].Addr().Is6())
	assert.True(t, strings.HasPrefix(b.addrs[1].String(), "192.0.2.1"))
}

func TestBridgeDestinationCloseEndsStream(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	newTestBridge(t, bridgeOptions(t, portalAddr(t, p), k, id), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	// A destination that speaks first and hangs up, as an HTTP/1.0 server
	// ending a close-delimited body does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("bye"))
		c.Close()
	}()

	o := newTestReverse(t, tagA, p)
	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(ln.Addr().String()))
	require.NoError(t, err)
	defer conn.Close()

	// The inviter never half-closes, yet sees the data and then EOF.
	got := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(conn)
		got <- b
	}()
	select {
	case b := <-got:
		assert.Equal(t, "bye", string(b))
	case <-time.After(5 * time.Second):
		t.Fatal("the inviter did not see EOF after the destination closed")
	}
}

func TestBridgeDestinationCloseDeliversEveryByte(t *testing.T) {
	k := newTestKeys(t)
	id := newShortID(t)
	p := newTestPortal(t, option.SamizdatPortalInboundOptions{})
	require.NoError(t, p.Configure(k.config(0, map[[8]byte]string{id: tagA})))
	newTestBridge(t, bridgeOptions(t, portalAddr(t, p), k, id), &dialRouter{})
	require.Eventually(t, func() bool { _, ok := p.Session(tagA); return ok }, 5*time.Second, 20*time.Millisecond)

	// Closing the stream the moment the destination is done must not cut
	// off what is still buffered on its way down.
	payload := make([]byte, 1<<20+12345)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Write(payload)
		c.Close()
	}()

	o := newTestReverse(t, tagA, p)
	conn, err := o.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr(ln.Addr().String()))
	require.NoError(t, err)
	defer conn.Close()

	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(conn)
		got <- result{b, err}
	}()
	select {
	case r := <-got:
		require.NoError(t, r.err)
		require.Len(t, r.b, len(payload))
		require.True(t, bytes.Equal(payload, r.b), "the inviter received different bytes")
	case <-time.After(15 * time.Second):
		t.Fatal("the inviter did not see EOF after the destination closed")
	}
}

// stallRouter writes to each routed conn until the writes stop making
// progress, which happens once the inviter's flow-control window is full,
// then fully closes the conn, as the router does when tearing a relay down.
// writeEnded gets the error the stalled Write returned, or nil if it was
// still blocked 5s after the Close.
type stallRouter struct {
	dialRouter
	writeEnded chan error
}

func (r *stallRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	go func() {
		var progress atomic.Int64
		writeErr := make(chan error, 1)
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := conn.Write(buf)
				progress.Add(int64(n))
				if err != nil {
					writeErr <- err
					return
				}
			}
		}()
		// Stalled once 300ms pass without a byte written.
		last := int64(-1)
		for cur := progress.Load(); cur != last; cur = progress.Load() {
			last = cur
			time.Sleep(300 * time.Millisecond)
		}
		conn.Close()
		select {
		case err := <-writeErr:
			r.writeEnded <- err
		case <-time.After(5 * time.Second):
			r.writeEnded <- nil
		}
		onClose(nil)
	}()
}

func TestBridgeCloseAbortsBlockedWrite(t *testing.T) {
	router := &stallRouter{writeEnded: make(chan error, 1)}
	b := &Bridge{
		Adapter: inbound.NewAdapter(constant.TypeSamizdatBridge, "bridge"),
		logger:  sblog.NewNOPFactory().Logger(),
		router:  router,
	}
	returned := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		b.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		(&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{Handler: handler})
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer raw.Close()
	cc, err := (&http2.Transport{}).NewClientConn(raw)
	require.NoError(t, err)
	defer cc.Close()

	pr, pw := io.Pipe()
	defer pw.Close()
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: "example.com:443"},
		Host:   "example.com:443",
		Header: http.Header{},
		Body:   pr,
	}
	resp, err := cc.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// The inviter never reads the response body.

	select {
	case err := <-router.writeEnded:
		require.Error(t, err, "a full Close left the stalled Write blocked")
	case <-time.After(30 * time.Second):
		t.Fatal("the router's writes never stalled")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge's handler did not return after the stream was closed")
	}
}
