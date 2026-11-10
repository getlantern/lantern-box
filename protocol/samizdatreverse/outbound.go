package samizdatreverse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/baderror"
	"github.com/sagernet/sing/common/bufio/deadline"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/sagernet/sing/service"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// ErrNoSession is returned by a reverse outbound's dials while its pairing
// has no live reversed session on the portal.
var ErrNoSession = errors.New("samizdat-reverse: no session")

// connectTimeout bounds how long a CONNECT waits for its response headers.
// It does not apply to the stream once established. A var so tests can
// shorten it.
var connectTimeout = 10 * time.Second

// sessionSource is what the outbound needs from the portal inbound.
type sessionSource interface {
	Session(reverseTag string) (*http2.ClientConn, bool)
}

// Outbound opens HTTP/2 CONNECT streams on the reversed session the portal
// holds for this outbound's pairing, keyed by the outbound's own tag.
type Outbound struct {
	outbound.Adapter
	ctx       context.Context
	logger    log.ContextLogger
	source    func() (sessionSource, bool)
	uotClient *uot.Client
}

// NewReverseOutbound creates a reverse outbound. The portal is resolved on
// each dial rather than at construction: inbounds are not outbound
// dependencies, and the portal may be configured after this outbound starts.
func NewReverseOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatReverseOutboundOptions) (adapter.Outbound, error) {
	o := &Outbound{
		Adapter: outbound.NewAdapter(constant.TypeSamizdatReverse, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		ctx:     ctx,
		logger:  logger,
	}
	o.source = func() (sessionSource, bool) {
		inbounds := service.FromContext[adapter.InboundManager](o.ctx)
		if inbounds == nil {
			return nil, false
		}
		in, ok := inbounds.Get(options.Portal)
		if !ok {
			return nil, false
		}
		src, ok := in.(sessionSource)
		return src, ok
	}
	o.uotClient = &uot.Client{Dialer: reverseDialer{o}, Version: uot.Version}
	return o, nil
}

// session returns this pairing's live session, or ErrNoSession at once. Like
// unbounded's checkHelperConnected (protocol/unbounded/outbound.go), it never
// waits: route dials have no deadline, so a url-test group that picked this
// outbound would hang instead of moving on to its other members.
func (o *Outbound) session() (*http2.ClientConn, error) {
	src, ok := o.source()
	if !ok {
		return nil, ErrNoSession
	}
	cc, ok := src.Session(o.Tag())
	if !ok {
		return nil, ErrNoSession
	}
	return cc, nil
}

// DialContext opens a TCP stream, or a UoT stream for UDP, on the session.
// UDP to port 443 goes to a black hole instead (see blackHole).
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	sent := sniffedDestination(ctx, destination)
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination

	switch N.NetworkName(network) {
	case N.NetworkTCP:
		return o.connect(ctx, sent)
	case N.NetworkUDP:
		if destination.Port == 443 {
			return newBlackHole(destination), nil
		}
		if _, err := o.session(); err != nil {
			return nil, err
		}
		return o.uotClient.DialContext(ctx, network, destination)
	}
	return nil, fmt.Errorf("samizdat-reverse: unsupported network: %s", network)
}

// sniffedDestination swaps an IP TCP destination for the host sniffed from
// the connection (TLS SNI or HTTP Host), as sing-box's
// sniff_override_destination does: the helper's exit policy may reject IP
// literals and admit only allowlisted hostnames. A claimed host never
// unlocks the original IP, since the bridge resolves the name itself and
// dials the address it resolved.
func sniffedDestination(ctx context.Context, destination M.Socksaddr) M.Socksaddr {
	if !destination.IsIP() {
		return destination
	}
	metadata := adapter.ContextFrom(ctx)
	if metadata == nil || !M.IsDomainName(metadata.Domain) {
		return destination
	}
	return M.Socksaddr{Fqdn: metadata.Domain, Port: destination.Port}
}

// ListenPacket opens a UoT packet connection on the session, or a black
// hole for port 443.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	if destination.Port == 443 {
		return newBlackHole(destination), nil
	}
	if _, err := o.session(); err != nil {
		return nil, err
	}
	return o.uotClient.ListenPacket(ctx, destination)
}

// Close is a no-op: the portal owns the session.
func (o *Outbound) Close() error {
	return nil
}

// connect opens a CONNECT stream to destination. Errors never name the
// destination.
func (o *Outbound) connect(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	cc, err := o.session()
	if err != nil {
		return nil, err
	}

	// The stream's context is detached from ctx so that the caller ending ctx
	// once the relay finishes does not reset the stream mid-close. ctx and the
	// header timeout cancel it only until the response headers arrive.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	stopCaller := context.AfterFunc(ctx, cancelStream)
	headerTimer := time.AfterFunc(connectTimeout, cancelStream)

	pr, pw := io.Pipe()
	host := destination.String()
	req := (&http.Request{
		Method:        http.MethodConnect,
		URL:           &url.URL{Scheme: "https", Host: host},
		Host:          host,
		Header:        http.Header{},
		Body:          pr,
		ContentLength: -1,
	}).WithContext(streamCtx)

	resp, err := cc.RoundTrip(req)
	// Detach. If either canceler already fired, the stream is (or is about
	// to be) torn down, so fail the dial rather than hand back a dying conn.
	callerFired := !stopCaller()
	timerFired := !headerTimer.Stop()
	if err == nil && (callerFired || timerFired) {
		resp.Body.Close()
		err = context.Canceled
		if timerFired {
			err = context.DeadlineExceeded
		}
	}
	if err != nil {
		cancelStream()
		pw.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("samizdat-reverse: CONNECT: %w", ctx.Err())
		}
		if timerFired {
			return nil, fmt.Errorf("samizdat-reverse: CONNECT: no response within %s: %w", connectTimeout, context.DeadlineExceeded)
		}
		// A session that fails mid-CONNECT returns the conn's *net.OpError,
		// which names both ends of the session.
		return nil, fmt.Errorf("samizdat-reverse: CONNECT: %w", addrFree(err))
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancelStream()
		pw.Close()
		return nil, fmt.Errorf("samizdat-reverse: CONNECT returned status %d", resp.StatusCode)
	}
	return &wrapConn{Conn: &streamConn{
		body:   resp.Body,
		pw:     pw,
		cancel: cancelStream,
		remote: destination,
	}}, nil
}

// reverseDialer gives the UoT client the outbound's TCP CONNECT path.
type reverseDialer struct{ o *Outbound }

func (d reverseDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return d.o.connect(ctx, destination)
}

func (d reverseDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

// streamConn is a net.Conn over one CONNECT stream: reads come from the
// response body, writes go to the request body pipe. Like sing-box's
// v2rayhttp HTTP2Conn it has no deadlines of its own and asks sing-box to add
// a read deadline instead.
type streamConn struct {
	body       io.ReadCloser
	pw         *io.PipeWriter
	cancel     context.CancelFunc
	remote     M.Socksaddr
	closeOnce  sync.Once
	writeClose sync.Once
}

func (c *streamConn) Read(b []byte) (int, error)  { return c.body.Read(b) }
func (c *streamConn) Write(b []byte) (int, error) { return c.pw.Write(b) }

// CloseWrite ends the request body, which net/http2 sends as END_STREAM,
// leaving the response side open.
func (c *streamConn) CloseWrite() error {
	var err error
	c.writeClose.Do(func() { err = c.pw.Close() })
	return err
}

func (c *streamConn) Close() error {
	c.closeOnce.Do(func() {
		c.CloseWrite()
		c.body.Close()
		c.cancel()
	})
	return nil
}

func (c *streamConn) LocalAddr() net.Addr                { return M.Socksaddr{} }
func (c *streamConn) RemoteAddr() net.Addr               { return c.remote }
func (c *streamConn) SetDeadline(t time.Time) error      { return os.ErrInvalid }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return os.ErrInvalid }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return os.ErrInvalid }
func (c *streamConn) NeedAdditionalReadDeadline() bool   { return true }

// wrapConn normalizes HTTP/2 read errors on a CONNECT stream conn. Copied
// from protocol/samizdat/conn.go (unexported there), minus the inbound byte
// counter: a torn-down stream surfaces golang.org/x/net/http2 errors that
// baderror.WrapH2 maps to net.ErrClosed, so the router treats them as an
// ordinary close. It hides Upstream() so sing-box cannot read past it, and
// therefore forwards the read-deadline hint and half-close explicitly.
type wrapConn struct {
	net.Conn
}

func (c *wrapConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	// A session that fails mid-stream surfaces its conn's *net.OpError,
	// which names both ends of the session, and the router logs it.
	return n, addrFree(baderror.WrapH2(err))
}

func (c *wrapConn) NeedAdditionalReadDeadline() bool {
	return deadline.NeedAdditionalReadDeadline(c.Conn)
}

func (c *wrapConn) CloseWrite() error {
	return N.CloseWrite(c.Conn)
}

// blackHole is the conn for UDP to port 443, which is QUIC: it never touches
// the session, discards writes, and blocks reads until Close or a read
// deadline. The helper's exit may admit only allowlisted hostnames, and
// QUIC carries none we can rely on, so refusing it makes the browser fall
// back to TCP, which carries the sniffed hostname. Dropping silently rather
// than returning an error keeps a group from counting a policy decision as
// a helper failure.
type blackHole struct {
	remote    M.Socksaddr
	closed    chan struct{}
	closeOnce sync.Once

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	// readWake is closed and replaced whenever the read deadline changes,
	// so a blocked read rechecks it.
	readWake chan struct{}
}

var (
	_ net.Conn       = (*blackHole)(nil)
	_ net.PacketConn = (*blackHole)(nil)
)

func newBlackHole(remote M.Socksaddr) *blackHole {
	return &blackHole{remote: remote, closed: make(chan struct{}), readWake: make(chan struct{})}
}

func (c *blackHole) Read([]byte) (int, error) { return 0, c.waitRead() }

func (c *blackHole) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, c.waitRead() }

func (c *blackHole) waitRead() error {
	for {
		c.mu.Lock()
		deadline, wake := c.readDeadline, c.readWake
		c.mu.Unlock()
		select {
		case <-c.closed:
			return net.ErrClosed
		default:
		}
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			expired = timer.C
		}
		var err error
		select {
		case <-c.closed:
			err = net.ErrClosed
		case <-expired:
			err = os.ErrDeadlineExceeded
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if err != nil {
			return err
		}
	}
}

func (c *blackHole) Write(b []byte) (int, error) { return c.write(b) }

func (c *blackHole) WriteTo(b []byte, _ net.Addr) (int, error) { return c.write(b) }

func (c *blackHole) write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return len(b), nil
}

func (c *blackHole) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *blackHole) LocalAddr() net.Addr  { return M.Socksaddr{} }
func (c *blackHole) RemoteAddr() net.Addr { return c.remote }

func (c *blackHole) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *blackHole) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	close(c.readWake)
	c.readWake = make(chan struct{})
	c.mu.Unlock()
	return nil
}

func (c *blackHole) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}
