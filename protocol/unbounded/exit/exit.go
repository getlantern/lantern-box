//go:build with_quic

package exit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"sync"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/tls"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/common/logger"

	"github.com/getlantern/lantern-box/protocol/internal/failureclass"
	"github.com/getlantern/lantern-box/protocol/internal/uotrouter"
)

// ErrUnavailable is what New returns in a build without with_quic.
var ErrUnavailable = errors.New("unbounded exit: built without with_quic")

// InboundTag is the inbound every direct exit stream and flow is routed as.
const InboundTag = "unbounded-exit"

// Exit serves a volunteer's direct exits: Hysteria2 over each ICE path broflake hands it, with
// each stream and flow routed into the current exit box as inbound unbounded-exit.
type Exit struct {
	ctx    context.Context
	cancel context.CancelFunc
	logger logger.ContextLogger
	cert   *certificate
	opts   *UBClientcore.ExitOptions

	mu         sync.Mutex
	closed     bool
	enabled    bool
	router     adapter.ConnectionRouterEx
	validUntil time.Time
	// expiry clears the router at validUntil. routerGen counts SetRouter calls, so an expiry
	// that fires after a later one leaves its router alone.
	expiry    *time.Timer
	routerGen uint64
	// revoked is closed when the exit stops being servable, ending its sessions. A new
	// channel replaces it once it is servable again.
	revoked chan struct{}

	now     func() time.Time
	metrics *exitMetrics
}

// New makes an exit, off and with no router, and its certificate, which it keeps for its life.
func New(ctx context.Context, lg logger.ContextLogger) (*Exit, error) {
	cert, err := newCertificate()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	revoked := make(chan struct{})
	close(revoked)
	e := &Exit{
		ctx:     ctx,
		cancel:  cancel,
		logger:  lg,
		cert:    cert,
		revoked: revoked,
		now:     time.Now,
		metrics: newExitMetrics(),
	}
	e.opts = &UBClientcore.ExitOptions{
		Ready:     e.ready,
		Params:    e.params,
		Serve:     e.serve,
		OnSession: func(directExit bool) { e.metrics.session(directExit) },
	}
	return e, nil
}

// SetEnabled sets the direct_exit flag. Turning it off also ends running sessions.
func (e *Exit) SetEnabled(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = on
	e.updateRevokedLocked()
}

// SetRouter sets the current exit box's router and how long its registration lasts. nil
// clears it and ends running sessions, as validUntil passing does.
func (e *Exit) SetRouter(r adapter.ConnectionRouterEx, validUntil time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routerGen++
	if e.expiry != nil {
		e.expiry.Stop()
		e.expiry = nil
	}
	if r != nil {
		// The UoT router logs each flow's destination at info, so it gets no logger.
		r = uotrouter.New(r, sblog.NewNOPFactory().Logger())
		gen := e.routerGen
		e.expiry = time.AfterFunc(validUntil.Sub(e.now()), func() { e.expire(gen) })
	}
	e.router, e.validUntil = r, validUntil
	e.updateRevokedLocked()
}

// expire clears the router set by SetRouter call gen, unless a later one replaced it.
func (e *Exit) expire(gen uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.routerGen != gen {
		return
	}
	e.router, e.validUntil, e.expiry = nil, time.Time{}, nil
	e.updateRevokedLocked()
}

// Options returns the hooks for broflake's WebRTCOptions.Exit, the same for the exit's life.
func (e *Exit) Options() *UBClientcore.ExitOptions { return e.opts }

// Close ends running sessions, and the exit serves none after.
func (e *Exit) Close() {
	e.mu.Lock()
	e.closed = true
	if e.expiry != nil {
		e.expiry.Stop()
		e.expiry = nil
	}
	e.updateRevokedLocked()
	e.mu.Unlock()
	e.cancel()
}

// updateRevokedLocked closes revoked when the exit can't serve, and opens a new one when it
// can again. Expiry revokes through the router it clears.
func (e *Exit) updateRevokedLocked() {
	servable := !e.closed && e.enabled && e.router != nil
	select {
	case <-e.revoked:
		if servable {
			e.revoked = make(chan struct{})
		}
	default:
		if !servable {
			close(e.revoked)
		}
	}
}

func (e *Exit) ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && e.enabled && e.router != nil && e.now().Before(e.validUntil)
}

// currentRouter is the router to route a stream or flow through, or nil once its registration
// has run out, even before its expiry has cleared it.
func (e *Exit) currentRouter() adapter.ConnectionRouterEx {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.now().Before(e.validUntil) {
		return nil
	}
	return e.router
}

func (e *Exit) params() (pin, password string, ok bool) {
	if !e.ready() {
		return "", "", false
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", false
	}
	return e.cert.pin, base64.RawURLEncoding.EncodeToString(b), true
}

// serve is clientcore's Serve hook: one Hysteria2 service for one session's path. It returns
// once ctx ends, the path's reads fail, or the exit is revoked, having closed the service and
// conn.
func (e *Exit) serve(ctx context.Context, conn net.PacketConn, password string) {
	e.mu.Lock()
	revoked := e.revoked
	e.mu.Unlock()
	defer func() { _ = conn.Close() }()
	select {
	case <-revoked:
		return
	default:
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	// The path's reads fail once its session ends; watching them ends serve with it.
	conn = &watchedConn{PacketConn: conn, dead: cancel}

	// The TLS and service errors come from the certificate and constants alone, so they can
	// be logged whole. Start's could name the path, so it logs only a class.
	tlsConfig, err := tls.NewServer(ctx, e.logger, option.InboundTLSOptions{
		Enabled:     true,
		ALPN:        badoption.Listable[string]{"h3"},
		Certificate: badoption.Listable[string]{e.cert.certPEM},
		Key:         badoption.Listable[string]{e.cert.keyPEM},
	})
	if err != nil {
		e.logger.WarnContext(ctx, "unbounded exit: TLS config: ", err)
		return
	}
	if err := tlsConfig.Start(); err != nil {
		e.logger.WarnContext(ctx, "unbounded exit: TLS start: ", err)
		return
	}
	defer func() { _ = tlsConfig.Close() }()
	svc, err := hysteria2.NewService[int](serviceOptions(ctx, tlsConfig, &handler{exit: e}))
	if err != nil {
		e.logger.WarnContext(ctx, "unbounded exit: service: ", err)
		return
	}
	svc.UpdateUsers([]int{0}, []string{password})
	if err := svc.Start(conn); err != nil {
		e.logger.WarnContext(ctx, "unbounded exit: start: ", failureclass.Of(err))
		_ = svc.Close()
		return
	}
	select {
	case <-ctx.Done():
	case <-revoked:
	}
	_ = svc.Close()
}

// serviceOptions forces BBR: with IgnoreClientBandwidth and no receive rate the service
// answers every client RxAuto, so Brutal is never negotiated. No masquerade: a client that
// fails authentication gets a bare 404. The service gets no logger: it logs the errors its
// sessions end with, which quote a client's close reason, text the client chose. The handler
// logs a failure's class instead.
func serviceOptions(ctx context.Context, tlsConfig tls.ServerConfig, h hysteria2.ServerHandler) hysteria2.ServiceOptions {
	return hysteria2.ServiceOptions{
		Context:               ctx,
		Logger:                sblog.NewNOPFactory().Logger(),
		TLSConfig:             tlsConfig,
		IgnoreClientBandwidth: true,
		SendBPS:               0,
		ReceiveBPS:            0,
		UDPTimeout:            2 * time.Minute,
		Handler:               h,
	}
}

// watchedConn calls dead once a read fails: the ICE session under it has ended.
type watchedConn struct {
	net.PacketConn
	dead func()
}

func (c *watchedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err != nil {
		c.dead()
	}
	return n, addr, err
}
