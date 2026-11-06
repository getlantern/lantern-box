package samizdatreverse

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
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
)

// PortalConfig is what the portal serves: where it listens, the samizdat
// server identity, and which helper each accepted short ID belongs to.
type PortalConfig struct {
	ListenPort uint16
	PrivateKey []byte // X25519, 32 bytes
	CertPEM    []byte
	KeyPEM     []byte
	Pairings   map[[8]byte]string // accepted short ID -> reverse outbound tag
	// Previous, if set, is an identity the portal also accepts new sessions
	// under until Previous.Until, so that helpers still dialing with the
	// identity the portal held before this one can reconnect while they
	// learn the new one. Its key must differ from PrivateKey.
	Previous *PreviousIdentity
}

// PreviousIdentity is a samizdat server identity the portal accepts new
// sessions under for a while after it has moved to another.
type PreviousIdentity struct {
	PrivateKey []byte // X25519, 32 bytes
	CertPEM    []byte
	KeyPEM     []byte
	// Until is when the portal stops accepting new sessions under the
	// identity. Sessions it already holds under it go on.
	Until time.Time
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
	port uint16
	// cur is the identity new sessions authenticate under, and prev, when
	// set, one they may still authenticate under until prevUntil. retired
	// holds identities that take no new sessions but still hold live ones.
	// cur is nil exactly when ln is: while the portal is down.
	cur, prev *identity
	prevUntil time.Time
	prevTimer *time.Timer
	retired   []*identity
	ln        net.Listener
	// serveDone closes when the accept loop on ln exits.
	serveDone chan struct{}
	rebinds   atomic.Int32 // listener swaps by Rebind, for tests
	sessSeq   atomic.Uint64
	// beforePublish, when set, runs in a session handler after HTTP/2 has
	// started and before the session is published. Tests use it to line up
	// concurrent handlers.
	beforePublish func()
	// beforeListen, when set, runs just before each bind. Tests use it to
	// take the port first.
	beforeListen func(port uint16)

	// routes is what the accept path reads to pick an identity. It is nil
	// while the portal is down.
	routes atomic.Pointer[routes]
	// routing holds the accepted connections whose ClientHello is still
	// being read, so a stop can close them. routeWG counts route calls.
	routeMu sync.Mutex
	routing map[net.Conn]struct{}
	routeWG sync.WaitGroup

	// sessMu guards pairings and sessions. It is never held while blocking.
	sessMu   sync.Mutex
	pairings map[[8]byte]string
	sessions map[string]*portalSession
}

type portalSession struct {
	shortID [8]byte
	cc      *http2.ClientConn
	// seq orders sessions by arrival. confirmed is set once the helper has
	// answered a ping, before the session is published, and never after.
	seq       uint64
	confirmed bool
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
		routing:          make(map[net.Conn]struct{}),
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
		// addrFree keeps the listen port and addresses out of the log.
		p.logger.Error(constant.TypeSamizdatPortal, ": rebind after a network change failed, will retry on the next one: ", addrFree(err))
	}
}

// Rebind replaces the portal's listener with a new one on the same port,
// bound to the current default interface. The samizdat servers and their
// sessions are kept: only accepting moves to the new listener. If the portal
// is down, because its listener stopped serving or an earlier bind failed, it
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
	if p.cur == nil || !p.serving() {
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
	p.startServe(ln)
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

// Configure makes cfg the portal's configuration. A change of pairings
// alone keeps the listener and sessions, ends the sessions of pairings that
// are gone and stops their short IDs from authenticating. No pairings at all
// stops the portal. Configuring the current configuration again changes
// nothing, unless the listener has stopped serving on its own, in which case
// the portal is rebuilt.
//
// A change of key or certificate on the same port keeps the listener. New
// sessions authenticate under cfg's identity, and also under cfg.Previous
// until its Until passes. An identity the portal stops taking new sessions
// under keeps the sessions it holds until they end or their pairing is
// removed, so rotating the key ends no session.
//
// A change of port replaces the listener and every identity, which ends
// every session. The new port is bound before the old listener stops, so a
// failed bind returns an error and leaves the old configuration serving. A
// failed bind while the portal is down leaves it down with cfg wanted, and
// Rebind or Configure tries again. ListenPort 0 binds the port the portal
// last served on, if any, and otherwise any free port.
func (p *Portal) Configure(cfg PortalConfig) error {
	cfg.Pairings = maps.Clone(cfg.Pairings)
	if cfg.Previous != nil {
		prev := *cfg.Previous
		cfg.Previous = &prev
	}
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

	if cfg.Previous != nil && !time.Now().Before(cfg.Previous.Until) {
		cfg.Previous = nil
	}
	// samizdat only loads a certificate at the first handshake, so check
	// both identities here, before anything that serves now is touched.
	if err := checkIdentity("", cfg.PrivateKey, cfg.CertPEM, cfg.KeyPEM); err != nil {
		return nil, err
	}
	if prev := cfg.Previous; prev != nil {
		if err := checkIdentity("previous ", prev.PrivateKey, prev.CertPEM, prev.KeyPEM); err != nil {
			return nil, err
		}
		if bytes.Equal(prev.PrivateKey, cfg.PrivateKey) {
			// The current identity would take every session meant for it.
			return nil, fmt.Errorf("%s: previous private key is the current one", constant.TypeSamizdatPortal)
		}
	}

	ids := make([][8]byte, 0, len(cfg.Pairings))
	for id := range cfg.Pairings {
		ids = append(ids, id)
	}

	bindPort := cfg.ListenPort
	if bindPort == 0 {
		bindPort = p.port
	}
	running := p.cur != nil && p.serving()
	if running && bindPort == p.port {
		return p.reidentifyLocked(cfg, ids)
	}

	cur, err := p.newIdentity(cfg.PrivateKey, cfg.CertPEM, cfg.KeyPEM, ids)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", constant.TypeSamizdatPortal, err)
	}
	var prev *identity
	if pi := cfg.Previous; pi != nil {
		if prev, err = p.newIdentity(pi.PrivateKey, pi.CertPEM, pi.KeyPEM, ids); err != nil {
			closeIdentities(cur)
			return nil, fmt.Errorf("%s: previous identity: %w", constant.TypeSamizdatPortal, err)
		}
	}

	var stale []*portalSession
	var stopErr error
	stop := func() {
		stale = p.setPairings(nil)
		stopErr = p.stopLocked()
	}
	// A running portal on a different port keeps serving until the new
	// port is bound, so a failed bind leaves it as it was. One whose accept
	// loop has exited goes first: it may still hold its listener and has
	// nothing worth keeping.
	if p.cur != nil && !running {
		stop()
	}

	ln, err := p.listen(bindPort)
	if err != nil {
		closeIdentities(cur, prev)
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
	p.cur = cur
	p.setPrevLocked(prev, cfg.Previous)
	p.startServe(ln)
	return append(stale, newStale...), stopErr
}

// reidentifyLocked moves a portal serving on the port cfg wants to cfg's
// identities and pairings, keeping the listener. An identity cfg still
// names keeps its server and sessions, whether it was current or previous
// before. One it no longer names is retired. p.mu must be held.
func (p *Portal) reidentifyLocked(cfg PortalConfig, ids [][8]byte) ([]*portalSession, error) {
	held := []*identity{p.cur, p.prev}
	var made []*identity
	find := func(privateKey, certPEM, keyPEM []byte) (*identity, error) {
		for _, id := range held {
			if id != nil && id.is(privateKey, certPEM, keyPEM) {
				return id, nil
			}
		}
		id, err := p.newIdentity(privateKey, certPEM, keyPEM, ids)
		if err != nil {
			return nil, err
		}
		made = append(made, id)
		return id, nil
	}
	cur, err := find(cfg.PrivateKey, cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", constant.TypeSamizdatPortal, err)
	}
	var prev *identity
	if pi := cfg.Previous; pi != nil {
		if prev, err = find(pi.PrivateKey, pi.CertPEM, pi.KeyPEM); err != nil {
			closeIdentities(made...)
			return nil, fmt.Errorf("%s: previous identity: %w", constant.TypeSamizdatPortal, err)
		}
	}

	// Pairings first, so a newly added ID resolves to its tag by the time
	// samizdat accepts it.
	stale := p.setPairings(cfg.Pairings)
	p.cur = cur
	// The new routes are published before anything is retired, so nothing
	// is routed to a retired identity from here on.
	p.setPrevLocked(prev, cfg.Previous)
	for _, id := range held {
		if id != nil && id != cur && id != prev {
			p.retireLocked(id)
		}
	}
	// SetShortIDs only fails on an empty set, which was ruled out above.
	for _, id := range append([]*identity{cur, prev}, p.retired...) {
		if id != nil && !slices.Contains(made, id) {
			if err := id.server.SetShortIDs(ids); err != nil {
				return stale, fmt.Errorf("%s: %w", constant.TypeSamizdatPortal, err)
			}
		}
	}
	p.cfg = cfg
	return stale, nil
}

// checkIdentity checks a samizdat server identity. which names it in the
// error.
func checkIdentity(which string, privateKey, certPEM, keyPEM []byte) error {
	if len(privateKey) != 32 {
		return fmt.Errorf("%s: %sprivate key must be 32 bytes", constant.TypeSamizdatPortal, which)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("%s: %scertificate: %w", constant.TypeSamizdatPortal, which, err)
	}
	return nil
}

// setPrevLocked makes id the previous identity, taking new sessions until
// pi.Until, and publishes the routes. A nil id clears it. p.mu must be held.
func (p *Portal) setPrevLocked(id *identity, pi *PreviousIdentity) {
	if p.prevTimer != nil {
		p.prevTimer.Stop()
		p.prevTimer = nil
	}
	p.prev, p.prevUntil = id, time.Time{}
	if id != nil {
		p.prevUntil = pi.Until
		p.prevTimer = time.AfterFunc(time.Until(pi.Until), func() { p.expire(id) })
	}
	r := &routes{cur: p.cur, prev: p.prev, prevUntil: p.prevUntil}
	if old := p.routes.Load(); old != nil {
		r.stop, r.cancel = old.stop, old.cancel
	} else {
		r.stop, r.cancel = context.WithCancel(context.Background())
	}
	p.routes.Store(r)
}

// expire retires id once its time as the previous identity is up. The
// accept path stops routing to it at that time on its own; this frees it.
func (p *Portal) expire(id *identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A Configure may have kept id as the previous identity for longer.
	if p.closed || p.prev != id || time.Now().Before(p.prevUntil) {
		return
	}
	p.setPrevLocked(nil, nil)
	p.retireLocked(id)
}

// retireLocked retires id, keeping it while it holds sessions. p.mu must be
// held.
func (p *Portal) retireLocked(id *identity) {
	p.retired = slices.DeleteFunc(p.retired, (*identity).isClosed)
	if id.retire() {
		p.retired = append(p.retired, id)
	}
}

// serving reports whether the accept loop on the current listener is still
// running. p.mu must be held.
func (p *Portal) serving() bool {
	if p.serveDone == nil {
		return false
	}
	select {
	case <-p.serveDone:
		return false
	default:
		return true
	}
}

// stopLocked closes the listener and every identity, which ends every
// session, and waits for them. p.mu must be held.
func (p *Portal) stopLocked() error {
	if p.cur == nil {
		return nil
	}
	err := p.ln.Close()
	<-p.serveDone
	p.stopRoutingLocked()
	if p.prevTimer != nil {
		p.prevTimer.Stop()
		p.prevTimer = nil
	}
	p.routes.Store(nil)
	closeIdentities(append([]*identity{p.cur, p.prev}, p.retired...)...)
	p.cur, p.prev, p.prevUntil, p.retired = nil, nil, time.Time{}, nil
	p.ln, p.serveDone = nil, nil
	if errors.Is(err, net.ErrClosed) {
		// The listener had already failed, and the accept loop has said so.
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

// Session returns the HTTP/2 client of the live session for reverseTag, if
// it can take a new stream. The client is checked outside sessMu, as every
// call into an HTTP/2 client must be.
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

	if p.beforePublish != nil {
		p.beforePublish()
	}
	s := &portalSession{shortID: shortID, cc: cc, seq: p.sessSeq.Add(1)}
	if !p.publish(ctx, tag, s) {
		return
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

// publish stores s as the session for tag and closes the one it replaces.
// It reports false if s should be dropped instead.
//
// An empty slot is filled at once, which adds no latency to the first
// connect. A session published that way is unconfirmed. When the slot is
// taken, s must first answer a ping before it can replace the occupant: the
// bridge races IPv6 against IPv4 with the same short ID and closes the loser
// without serving HTTP/2, so an unconfirmed loser must never close a working
// session. The occupant is never inspected. Its HTTP/2 client can be stuck
// behind a write to a helper that stopped reading, and a confirmed
// replacement is exactly what restores traffic then. A confirmed s replaces
// an unconfirmed or older occupant. Between confirmed sessions the newer
// arrival wins, so a confirmation that finishes late cannot undo a newer
// reconnect. Only fields of the sessions are read under sessMu; the ping
// runs outside it, and the slot is evaluated again afterwards.
func (p *Portal) publish(ctx context.Context, tag string, s *portalSession) bool {
	for {
		p.sessMu.Lock()
		if p.pairings[s.shortID] != tag {
			p.sessMu.Unlock()
			return false
		}
		cur := p.sessions[tag]
		if cur == nil || (s.confirmed && (!cur.confirmed || cur.seq < s.seq)) {
			p.sessions[tag] = s
			p.sessMu.Unlock()
			if cur != nil {
				cur.cc.Close()
			}
			return true
		}
		if s.confirmed || (cur.confirmed && cur.seq > s.seq) {
			// A newer confirmed session holds the slot.
			p.sessMu.Unlock()
			return false
		}
		p.sessMu.Unlock()

		pingCtx, cancel := context.WithTimeout(ctx, p.confirmTimeout)
		err := s.cc.Ping(pingCtx)
		cancel()
		if err != nil {
			return false
		}
		s.confirmed = true
	}
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
