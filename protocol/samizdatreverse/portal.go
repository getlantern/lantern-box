package samizdatreverse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	"golang.org/x/net/http2"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/internal/physnet"

	samizdat "github.com/getlantern/samizdat"
)

// PortalConfig is what the portal serves: where it listens, the samizdat
// server identity, and which helper each accepted short ID belongs to.
type PortalConfig struct {
	ListenPort uint16
	PrivateKey []byte // X25519, 32 bytes
	CertPEM    []byte
	KeyPEM     []byte
	Pairings   map[[8]byte]string // accepted short ID -> reverse outbound tag
}

// sameServer reports whether a and b can share one listener and server, so
// that moving from one to the other only changes the accepted short IDs.
func (a PortalConfig) sameServer(b PortalConfig) bool {
	return a.ListenPort == b.ListenPort &&
		bytes.Equal(a.PrivateKey, b.PrivateKey) &&
		bytes.Equal(a.CertPEM, b.CertPEM) &&
		bytes.Equal(a.KeyPEM, b.KeyPEM)
}

// Portal is the inbound on an inviter's device that helpers dial into. Each
// authenticated samizdat session carries HTTP/2 with the roles reversed: the
// portal is the HTTP/2 client, and reverse outbounds open CONNECT streams on
// the session that Session returns for their tag. The portal holds at most
// one session per pairing; a newer one replaces the older.
type Portal struct {
	inbound.Adapter
	ctx              context.Context
	logger           log.ContextLogger
	masqueradeDomain string
	masqueradeAddr   string
	// confirmTimeout bounds the ping a session must answer before it
	// replaces a live one.
	confirmTimeout time.Duration
	// rebindDelay is how long InterfaceUpdated waits for further updates
	// before it rebinds, so a burst of updates causes one rebind.
	rebindDelay time.Duration

	rebindMu    sync.Mutex
	rebindTimer *time.Timer

	// mu serializes Configure and Close and guards the fields below it. It
	// is never taken by a session handler.
	mu     sync.Mutex
	closed bool
	// cfg is the configuration the portal wants to serve. It stays set
	// while the portal is down after a failed bind, so a later Rebind or
	// Configure can bring it back. It is empty only when nothing is wanted.
	cfg PortalConfig
	// port is the port the portal last served on. A rebuild of a
	// configuration with ListenPort 0 binds it again, so helpers keep
	// reaching the portal where they were told it is.
	port      uint16
	server    *samizdat.Server
	ln        net.Listener
	serveDone chan struct{}
	rebinds   atomic.Int32 // listener swaps by Rebind, for tests
	// beforeListen, when set, runs just before each bind. Tests use it to
	// take the port first.
	beforeListen func(port uint16)

	// sessMu guards pairings and sessions. It is never held while blocking.
	sessMu   sync.Mutex
	pairings map[[8]byte]string
	sessions map[string]*portalSession
}

type portalSession struct {
	shortID [8]byte
	cc      *http2.ClientConn
}

// NewPortal creates a portal inbound. It listens only once Configure gives
// it a pairing.
func NewPortal(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.SamizdatPortalInboundOptions) (adapter.Inbound, error) {
	return &Portal{
		Adapter:          inbound.NewAdapter(constant.TypeSamizdatPortal, tag),
		ctx:              ctx,
		logger:           logger,
		masqueradeDomain: options.MasqueradeDomain,
		masqueradeAddr:   options.MasqueradeAddr,
		confirmTimeout:   5 * time.Second,
		rebindDelay:      time.Second,
		sessions:         make(map[string]*portalSession),
	}, nil
}

// Start does nothing: the portal starts serving on Configure.
func (p *Portal) Start(stage adapter.StartStage) error {
	return nil
}

var _ adapter.InterfaceUpdateListener = (*Portal)(nil)

// InterfaceUpdated implements adapter.InterfaceUpdateListener. sing-box's
// network manager calls it on every inbound when the default interface
// changes. The listener was bound to the interface that was the default
// when it was created, so the portal rebinds after updates stop arriving
// for rebindDelay. It returns at once: the caller is the network monitor.
func (p *Portal) InterfaceUpdated() {
	p.rebindMu.Lock()
	defer p.rebindMu.Unlock()
	if p.rebindTimer == nil {
		p.rebindTimer = time.AfterFunc(p.rebindDelay, p.rebindAfterUpdate)
		return
	}
	p.rebindTimer.Reset(p.rebindDelay)
}

func (p *Portal) rebindAfterUpdate() {
	if err := p.Rebind(); err != nil {
		p.logger.Error(constant.TypeSamizdatPortal, ": rebind after a network change: ", err)
	}
}

// Rebind replaces the portal's listener with a new one on the same port,
// bound to the current default interface. The samizdat server and its
// sessions are kept: only accepting moves to the new listener. If the portal
// is down, because its server stopped serving or an earlier bind failed, it
// is rebuilt from the wanted configuration on the port it last served on. If
// the bind fails, the portal stays down, keeps that configuration and
// returns the error, and the next Rebind or Configure tries again. Rebind
// does nothing when no configuration is wanted or the portal is closed.
func (p *Portal) Rebind() error {
	p.mu.Lock()
	stale, err := p.rebindLocked()
	p.mu.Unlock()
	closeSessions(stale)
	return err
}

func (p *Portal) rebindLocked() ([]*portalSession, error) {
	if p.closed || len(p.cfg.Pairings) == 0 {
		return nil, nil
	}
	if p.server == nil || !p.serving() {
		return p.configureLocked(p.cfg)
	}
	p.ln.Close()
	<-p.serveDone
	ln, err := p.listen(p.port)
	if err != nil {
		// Down, but p.cfg and p.port are kept for the next attempt.
		stale := p.setPairings(nil)
		stopErr := p.stopLocked()
		return stale, errors.Join(err, stopErr)
	}
	p.startServe(p.server, ln)
	p.rebinds.Add(1)
	return nil, nil
}

// listen binds the portal's dual-stack listener on port.
func (p *Portal) listen(port uint16) (net.Listener, error) {
	if p.beforeListen != nil {
		p.beforeListen(port)
	}
	lc := net.ListenConfig{Control: physnet.ListenControl(p.ctx)}
	ln, err := lc.Listen(p.ctx, "tcp", net.JoinHostPort("::", strconv.Itoa(int(port))))
	if err != nil {
		return nil, fmt.Errorf("%s: listen: %w", constant.TypeSamizdatPortal, addrFree(err))
	}
	return ln, nil
}

// startServe runs server on ln and records both. p.mu must be held.
func (p *Portal) startServe(server *samizdat.Server, ln net.Listener) {
	p.server = server
	p.ln = ln
	p.port = uint16(ln.Addr().(*net.TCPAddr).Port)
	done := make(chan struct{})
	p.serveDone = done
	go func() {
		defer close(done)
		// net.ErrClosed means Rebind or Close closed the listener.
		if err := server.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			p.logger.Error(constant.TypeSamizdatPortal, " stopped serving: ", addrFree(err))
		}
	}()
}

// Close stops the portal and ends every session. Configure fails after it.
func (p *Portal) Close() error {
	p.rebindMu.Lock()
	if p.rebindTimer != nil {
		p.rebindTimer.Stop()
	}
	p.rebindMu.Unlock()
	p.mu.Lock()
	p.closed = true
	stale := p.setPairings(nil)
	err := p.stopLocked()
	p.cfg, p.port = PortalConfig{}, 0
	p.mu.Unlock()
	closeSessions(stale)
	return err
}

// Addr returns the address the portal listens on, or nil when it is not
// listening.
func (p *Portal) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return nil
	}
	return p.ln.Addr()
}

// Configure makes cfg the portal's configuration. A change of port, key or
// certificate replaces the server and listener. A change of pairings alone
// keeps them, ends the sessions of pairings that are gone and stops their
// short IDs from authenticating. No pairings at all stops the portal.
// Configuring the current configuration again changes nothing, unless the
// server has stopped serving on its own, in which case it is rebuilt.
//
// When the port changes, the new port is bound before the old server stops,
// so a failed bind returns an error and leaves the old configuration
// serving. A key or certificate change on the same port has to stop the old
// server first, so a failed bind there leaves the portal down with cfg
// wanted, and Rebind or Configure tries again. ListenPort 0 binds the port
// the portal last served on, if any, and otherwise any free port.
func (p *Portal) Configure(cfg PortalConfig) error {
	cfg.Pairings = maps.Clone(cfg.Pairings)
	p.mu.Lock()
	stale, err := p.configureLocked(cfg)
	p.mu.Unlock()
	closeSessions(stale)
	return err
}

func (p *Portal) configureLocked(cfg PortalConfig) ([]*portalSession, error) {
	if p.closed {
		return nil, errors.New(constant.TypeSamizdatPortal + ": closed")
	}

	if len(cfg.Pairings) == 0 {
		stale := p.setPairings(nil)
		err := p.stopLocked()
		p.cfg, p.port = PortalConfig{}, 0
		return stale, err
	}

	ids := make([][8]byte, 0, len(cfg.Pairings))
	for id := range cfg.Pairings {
		ids = append(ids, id)
	}

	if p.server != nil && p.cfg.sameServer(cfg) && p.serving() {
		// Pairings first, so a newly added ID resolves to its tag by the
		// time samizdat accepts it. SetShortIDs only fails on an empty set,
		// which was ruled out above.
		stale := p.setPairings(cfg.Pairings)
		if err := p.server.SetShortIDs(ids); err != nil {
			return stale, fmt.Errorf("%s: %w", constant.TypeSamizdatPortal, err)
		}
		p.cfg = cfg
		return stale, nil
	}

	server, err := samizdat.NewServer(samizdat.ServerConfig{
		PrivateKey:       cfg.PrivateKey,
		ShortIDs:         ids,
		CertPEM:          cfg.CertPEM,
		KeyPEM:           cfg.KeyPEM,
		MasqueradeDomain: p.masqueradeDomain,
		MasqueradeAddr:   p.masqueradeAddr,
		SessionHandler:   p.handleSession,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", constant.TypeSamizdatPortal, err)
	}

	bindPort := cfg.ListenPort
	if bindPort == 0 {
		bindPort = p.port
	}
	var stale []*portalSession
	var stopErr error
	stop := func() {
		stale = p.setPairings(nil)
		stopErr = p.stopLocked()
	}
	// A running server holding a different port keeps serving until the new
	// one is bound, so a failed bind leaves it as it was. One holding the
	// same port must go first, and so must one whose Serve loop has exited
	// (it may still hold its listener and has nothing worth keeping).
	running := p.server != nil && p.serving()
	if p.server != nil && (!running || bindPort == p.port) {
		stop()
		running = false
	}

	ln, err := p.listen(bindPort)
	if err != nil {
		server.Close()
		if !running {
			// Nothing is serving. Keep cfg wanted so Rebind or Configure
			// can bring the portal back.
			p.cfg = cfg
		}
		return stale, errors.Join(stopErr, err)
	}
	if running {
		stop()
	}

	newStale := p.setPairings(cfg.Pairings)
	p.cfg = cfg
	p.startServe(server, ln)
	return append(stale, newStale...), stopErr
}

// serving reports whether the current server's Serve loop is still running.
// p.mu must be held.
func (p *Portal) serving() bool {
	select {
	case <-p.serveDone:
		return false
	default:
		return true
	}
}

// stopLocked closes the server and listener, which ends every session, and
// waits for them. p.mu must be held.
func (p *Portal) stopLocked() error {
	if p.server == nil {
		return nil
	}
	err := p.server.Close()
	<-p.serveDone
	p.server, p.ln, p.serveDone = nil, nil, nil
	if errors.Is(err, net.ErrClosed) {
		// The listener had already failed, and Serve has said so.
		return nil
	}
	return addrFree(err)
}

// setPairings makes pairings the accepted pairings. It removes every session
// whose short ID is no longer paired with the tag it was stored under and
// returns them for the caller to close once it has released p.mu.
func (p *Portal) setPairings(pairings map[[8]byte]string) []*portalSession {
	var stale []*portalSession
	p.sessMu.Lock()
	p.pairings = pairings
	for tag, s := range p.sessions {
		if pairings[s.shortID] != tag {
			delete(p.sessions, tag)
			stale = append(stale, s)
		}
	}
	p.sessMu.Unlock()
	return stale
}

// closeSessions closes sessions in parallel: closing a TLS conn whose peer
// has stopped reading can block on its close_notify for seconds.
func closeSessions(sessions []*portalSession) {
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.cc.Close()
		}()
	}
	wg.Wait()
}

// addrFree strips the addresses a *net.OpError carries, keeping its
// operation and cause, so the error can be logged or returned without
// naming a peer.
func addrFree(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return fmt.Errorf("%s: %w", opErr.Op, opErr.Err)
	}
	return err
}

// Session returns the HTTP/2 client of the live session for reverseTag, if
// it can take a new stream.
func (p *Portal) Session(reverseTag string) (*http2.ClientConn, bool) {
	p.sessMu.Lock()
	s := p.sessions[reverseTag]
	p.sessMu.Unlock()
	if s == nil || !s.cc.CanTakeNewRequest() {
		return nil, false
	}
	return s.cc, true
}

// handleSession is the samizdat SessionHandler. It runs an HTTP/2 client on
// the session, publishes it under the pairing's tag, and holds it until the
// session or its connection ends.
func (p *Portal) handleSession(ctx context.Context, conn net.Conn, shortID [8]byte) {
	p.sessMu.Lock()
	tag, ok := p.pairings[shortID]
	p.sessMu.Unlock()
	if !ok {
		return
	}

	wc := &closeNotifyConn{Conn: conn, done: make(chan struct{})}
	cc, err := (&http2.Transport{ReadIdleTimeout: 30 * time.Second, PingTimeout: 30 * time.Second}).NewClientConn(wc)
	if err != nil {
		// err can be a *net.OpError naming the helper's address.
		p.logger.Debug(constant.TypeSamizdatPortal, ": could not start HTTP/2 on a session")
		return
	}
	defer cc.Close()

	// A session only replaces a live one once the helper has answered a
	// ping on it. The bridge races IPv6 against IPv4 with the same short ID
	// and closes the loser without serving HTTP/2. Without this check, a
	// loser whose handshake finished late would close the working session.
	// With no live session, publish at once: that adds no latency to the
	// first connect, and an unconfirmed session is still replaced by any
	// later one that answers.
	p.sessMu.Lock()
	cur := p.sessions[tag]
	p.sessMu.Unlock()
	if cur != nil && cur.cc.CanTakeNewRequest() {
		pingCtx, cancel := context.WithTimeout(ctx, p.confirmTimeout)
		err := cc.Ping(pingCtx)
		cancel()
		if err != nil {
			return
		}
	}

	s := &portalSession{shortID: shortID, cc: cc}
	p.sessMu.Lock()
	if p.pairings[shortID] != tag {
		p.sessMu.Unlock()
		return
	}
	old := p.sessions[tag]
	p.sessions[tag] = s
	p.sessMu.Unlock()
	if old != nil {
		old.cc.Close()
	}

	select {
	case <-ctx.Done():
	case <-wc.done:
	}

	p.sessMu.Lock()
	if p.sessions[tag] == s {
		delete(p.sessions, tag)
	}
	p.sessMu.Unlock()
}

// closeNotifyConn closes done when the HTTP/2 client closes the connection,
// which it does on cc.Close and when its read loop fails.
type closeNotifyConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *closeNotifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
