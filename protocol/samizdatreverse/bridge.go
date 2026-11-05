package samizdatreverse

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	sbopt "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/internal/uotrouter"
	"github.com/getlantern/lantern-box/tracker/peerconn"

	samizdat "github.com/getlantern/samizdat"
)

const (
	// eyeballsDelay is IPv6's head start over IPv4.
	eyeballsDelay = 250 * time.Millisecond

	backoffMin = time.Second
	backoffMax = 5 * time.Minute
	// backoffResetAfter is how long a session must last for the backoff to
	// start over.
	backoffResetAfter = time.Minute

	// The portal's HTTP/2 client pings on the same schedule, so either end
	// notices a dead session within a minute.
	pingAfterIdle = 30 * time.Second
	pingTimeout   = 30 * time.Second
)

// Bridge is the inbound on a helper's device that dials out to an inviter's
// portal. On the samizdat session it dials it serves HTTP/2 with the roles
// reversed: the portal opens CONNECT streams, and the bridge routes each as
// an inbound connection, so the inviter's traffic exits from the helper. It
// keeps one session up at a time, redialing with backoff when it ends.
type Bridge struct {
	inbound.Adapter
	ctx             context.Context
	cancel          context.CancelFunc
	logger          log.ContextLogger
	router          adapter.ConnectionRouterEx
	dialerOptions   sbopt.DialerOptions
	addrs           []netip.AddrPort // IPv6 first
	shortID         [8]byte
	clients         map[netip.AddrPort]*samizdat.Client
	shutdownTimeout time.Duration // time to wait for a routed conn after its stream ends; 0 = 5s

	// Test seams. dial is the TCP dialer under samizdat; Start builds it from
	// the dialer options unless a test set it. attempt dials and serves one
	// session, returning how long it lasted.
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	attempt func(ctx context.Context) (time.Duration, error)
	sleep   func(ctx context.Context, d time.Duration) bool
	jitter  func(d time.Duration) time.Duration
	now     func() time.Time

	mu      sync.Mutex
	started bool
	runDone chan struct{}
}

// NewBridge creates a bridge inbound. It dials only once started.
func NewBridge(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatBridgeInboundOptions) (adapter.Inbound, error) {
	addrs, err := parseAddrs(options.Addrs)
	if err != nil {
		return nil, err
	}
	pubKey, err := decodeHex("public_key", options.PublicKey, 32)
	if err != nil {
		return nil, err
	}
	sid, err := decodeHex("short_id", options.ShortID, 8)
	if err != nil {
		return nil, err
	}
	pin, err := decodeHex("cert_spki_sha256", options.CertSPKISHA256, 32)
	if err != nil {
		return nil, err
	}
	if options.ServerName == "" {
		return nil, errors.New(constant.TypeSamizdatBridge + ": server_name is required")
	}

	ctx, cancel := context.WithCancel(ctx)
	b := &Bridge{
		Adapter:       inbound.NewAdapter(constant.TypeSamizdatBridge, tag),
		ctx:           ctx,
		cancel:        cancel,
		logger:        logger,
		router:        uotrouter.New(router, logger),
		dialerOptions: options.DialerOptions,
		addrs:         addrs,
		clients:       make(map[netip.AddrPort]*samizdat.Client, len(addrs)),
		sleep:         sleepCtx,
		jitter:        fullJitter,
		now:           time.Now,
	}
	copy(b.shortID[:], sid)
	b.attempt = b.runSession

	for _, addr := range addrs {
		client, err := samizdat.NewClient(samizdat.ClientConfig{
			ServerAddr:           addr.String(),
			ServerName:           options.ServerName,
			PublicKey:            pubKey,
			ShortID:              b.shortID,
			ServerCertSPKISHA256: pin,
			Fingerprint:          options.Fingerprint,
			TCPFragmentation:     true,
			Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
				return b.dial(ctx, network, address)
			},
		})
		if err != nil {
			cancel()
			// The config errors name lengths and fields, never values.
			return nil, fmt.Errorf("%s: %w", constant.TypeSamizdatBridge, err)
		}
		b.clients[addr] = client
	}
	return b, nil
}

// parseAddrs parses one or two "ip:port" addresses, at most one per family,
// and orders IPv6 first. Errors never repeat an address.
func parseAddrs(raw []string) ([]netip.AddrPort, error) {
	if len(raw) == 0 || len(raw) > 2 {
		return nil, fmt.Errorf("%s: addrs must hold one or two addresses, got %d", constant.TypeSamizdatBridge, len(raw))
	}
	addrs := make([]netip.AddrPort, 0, len(raw))
	seen := map[int]bool{}
	for i, s := range raw {
		ap, err := netip.ParseAddrPort(s)
		if err != nil || ap.Port() == 0 {
			return nil, fmt.Errorf("%s: addrs[%d] is not an ip:port", constant.TypeSamizdatBridge, i)
		}
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if seen[family(ap)] {
			return nil, fmt.Errorf("%s: addrs holds two addresses of one family", constant.TypeSamizdatBridge)
		}
		seen[family(ap)] = true
		addrs = append(addrs, ap)
	}
	if len(addrs) == 2 && family(addrs[0]) > family(addrs[1]) {
		addrs[0], addrs[1] = addrs[1], addrs[0]
	}
	return addrs, nil
}

// decodeHex decodes a hex option of exactly size bytes. Errors never repeat
// the value.
func decodeHex(name, s string, size int) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != size {
		return nil, fmt.Errorf("%s: %s must be %d hex characters", constant.TypeSamizdatBridge, name, 2*size)
	}
	return b, nil
}

// Start starts the dial loop.
func (b *Bridge) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started || b.ctx.Err() != nil {
		return nil
	}
	if b.dial == nil {
		d, err := dialer.New(b.ctx, b.dialerOptions, false)
		if err != nil {
			return fmt.Errorf("%s: %w", constant.TypeSamizdatBridge, err)
		}
		b.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.DialContext(ctx, network, M.ParseSocksaddr(address))
		}
	}
	b.started = true
	done := make(chan struct{})
	b.runDone = done
	go func() {
		defer close(done)
		b.run(b.ctx)
	}()
	return nil
}

// Close stops the dial loop and ends the live session.
func (b *Bridge) Close() error {
	b.cancel()
	b.mu.Lock()
	done := b.runDone
	b.mu.Unlock()
	if done != nil {
		<-done
	}
	for _, c := range b.clients {
		c.Close()
	}
	return nil
}

// run dials and serves sessions until ctx ends, backing off between them:
// from backoffMin, doubling to backoffMax with full jitter, and starting over
// after a session that lasted longer than backoffResetAfter. A pin mismatch
// backs off like any other failure.
func (b *Bridge) run(ctx context.Context) {
	backoff := backoffMin
	for {
		lasted, err := b.attempt(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, samizdat.ErrCertPinMismatch):
			b.logger.Warn(constant.TypeSamizdatBridge, ": the portal's certificate does not match the pin")
		case err != nil:
			// err can name the portal's address.
			b.logger.Debug(constant.TypeSamizdatBridge, ": could not reach the portal")
		default:
			b.logger.Debug(constant.TypeSamizdatBridge, ": session ended after ", lasted.Round(time.Second))
		}
		if lasted > backoffResetAfter {
			backoff = backoffMin
		}
		wait := b.jitter(backoff)
		backoff = min(2*backoff, backoffMax)
		if !b.sleep(ctx, wait) {
			return
		}
	}
}

// runSession dials the portal and serves HTTP/2 on the session until it
// ends. It returns how long the session lasted, or an error if none was
// established.
func (b *Bridge) runSession(ctx context.Context) (time.Duration, error) {
	conn, err := dialHappyEyeballs(ctx, b.addrs, eyeballsDelay, func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		return b.clients[addr].DialSession(ctx)
	})
	if err != nil {
		return 0, err
	}
	start := b.now()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	b.logger.Debug(constant.TypeSamizdatBridge, ": session established")
	server := &http2.Server{
		IdleTimeout:     0,
		ReadIdleTimeout: pingAfterIdle,
		PingTimeout:     pingTimeout,
	}
	server.ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: b})
	return b.now().Sub(start), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// fullJitter returns a uniformly random duration in [0, d].
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d + 1)
}

// ServeHTTP answers a CONNECT from the portal with 200 and routes the stream
// to its destination.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	destination := r.Host
	if destination == "" {
		http.Error(w, "no destination", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	stream := &bridgeStream{body: r.Body, w: w, flusher: flusher, remote: M.ParseSocksaddr(r.RemoteAddr)}
	// No write may reach w once this handler returns.
	defer stream.shutdown()
	b.handleConnection(r.Context(), stream, destination)
}

// handleConnection routes one CONNECT stream through the sing-box router and
// blocks until it is done, since the stream ends when ServeHTTP returns.
// Copied from protocol/samizdat/inbound.go (Inbound.handleConnection) with
// the bridge's own short ID on the peerconn events, and without logging the
// destination or routing errors, which can name it.
func (b *Bridge) handleConnection(ctx context.Context, conn net.Conn, destination string) {
	// Bytes are counted only for a listener to read.
	notify := peerconn.Acquire()
	var bytes *peerconn.Counter
	if notify != nil {
		bytes = &peerconn.Counter{}
	}
	conn = &countingConn{wrapConn: wrapConn{Conn: conn}, bytes: bytes}

	var metadata adapter.InboundContext
	metadata.Inbound = b.Tag()
	metadata.InboundType = b.Type()
	metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr()).Unwrap()
	metadata.Destination = M.ParseSocksaddr(destination)

	// Acquired once so the close goes to the listener that saw the accept.
	source := metadata.Source.String()
	// Written by the routing callback and read by the deferred close, which
	// can run without the callback having fired when shutdown times out.
	var rejected atomic.Bool
	if notify != nil {
		shortID := b.shortID
		notify(peerconn.Event{State: +1, Source: source, Destination: destination, Bytes: bytes, ShortID: shortID})
		defer func() {
			evt := peerconn.Event{State: -1, Source: source, Bytes: bytes, ShortID: shortID}
			if rejected.Load() {
				evt.Destination, evt.Rejected = destination, true
			}
			notify(evt)
		}()
	}

	done := make(chan struct{})
	b.router.RouteConnectionEx(ctx, conn, metadata, func(err error) {
		if err != nil {
			rejected.Store(rule.IsRejected(err))
			b.logger.DebugContext(ctx, constant.TypeSamizdatBridge, ": a routed connection failed")
		}
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
		// The stream or session ended. Close the conn so the copy goroutines
		// error out, and wait for them: none may write to the stream after
		// ServeHTTP returns.
		conn.Close()
		timeout := b.shutdownTimeout
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		timer := time.NewTimer(timeout)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			b.logger.ErrorContext(ctx, constant.TypeSamizdatBridge, ": timed out waiting for a routed connection to finish")
		}
	}
}

// countingConn is wrapConn plus the inbound byte counter, as in
// protocol/samizdat/conn.go: reads are bytes up from the inviter, writes bytes
// down to it.
type countingConn struct {
	wrapConn
	bytes *peerconn.Counter
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.wrapConn.Read(b)
	if c.bytes != nil {
		c.bytes.AddUp(n)
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.wrapConn.Write(b)
	if c.bytes != nil {
		c.bytes.AddDown(n)
	}
	return n, err
}

// bridgeStream is a net.Conn over one CONNECT stream the bridge serves:
// reads come from the request body, writes go to the response.
type bridgeStream struct {
	body    io.ReadCloser
	w       http.ResponseWriter
	flusher http.Flusher
	remote  M.Socksaddr

	mu        sync.Mutex // held across each write; shutdown takes it to wait one out
	closed    atomic.Bool
	closeOnce sync.Once
}

func (c *bridgeStream) Read(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.body.Read(b)
}

func (c *bridgeStream) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	n, err := c.w.Write(b)
	if err == nil && c.flusher != nil {
		c.flusher.Flush()
	}
	return n, err
}

// Close stops reads and writes. The response ends when ServeHTTP returns.
func (c *bridgeStream) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.body.Close()
	})
	return nil
}

// CloseWrite closes the whole stream. A server cannot end its response
// while it still reads the request: net/http2 resets the stream when the
// handler returns. Ending the stream when the destination is done sending
// lets the inviter see EOF; leaving it open would hang a client that waits
// for EOF before it ends its own side.
func (c *bridgeStream) CloseWrite() error {
	return c.Close()
}

// shutdown marks the stream closed and waits for an in-flight write.
func (c *bridgeStream) shutdown() {
	c.mu.Lock()
	c.closed.Store(true)
	c.mu.Unlock()
}

func (c *bridgeStream) LocalAddr() net.Addr                { return M.Socksaddr{} }
func (c *bridgeStream) RemoteAddr() net.Addr               { return c.remote }
func (c *bridgeStream) SetDeadline(t time.Time) error      { return os.ErrInvalid }
func (c *bridgeStream) SetReadDeadline(t time.Time) error  { return os.ErrInvalid }
func (c *bridgeStream) SetWriteDeadline(t time.Time) error { return os.ErrInvalid }
func (c *bridgeStream) NeedAdditionalReadDeadline() bool   { return true }
