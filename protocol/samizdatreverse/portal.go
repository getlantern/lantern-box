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

	// mu serializes Configure and Close and guards the fields below it. It
	// is never taken by a session handler.
	mu        sync.Mutex
	closed    bool
	cfg       PortalConfig
	server    *samizdat.Server
	ln        net.Listener
	serveDone chan struct{}

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
		sessions:         make(map[string]*portalSession),
	}, nil
}

// Start does nothing: the portal starts serving on Configure.
func (p *Portal) Start(stage adapter.StartStage) error {
	return nil
}

// Close stops the portal and ends every session. Configure fails after it.
func (p *Portal) Close() error {
	p.mu.Lock()
	p.closed = true
	stale := p.setPairings(nil)
	err := p.stopLocked()
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
// server first, so a failed bind there leaves the portal stopped.
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
		p.cfg = PortalConfig{}
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

	var stale []*portalSession
	var stopErr error
	stop := func() {
		stale = p.setPairings(nil)
		stopErr = p.stopLocked()
		p.cfg = PortalConfig{}
	}
	// The old listener must go first only when it holds the port the new
	// one needs.
	portChange := p.ln == nil || cfg.ListenPort != p.cfg.ListenPort
	if !portChange {
		stop()
	}

	lc := net.ListenConfig{Control: physnet.ListenControl(p.ctx)}
	ln, err := lc.Listen(p.ctx, "tcp", net.JoinHostPort("::", strconv.Itoa(int(cfg.ListenPort))))
	if err != nil {
		server.Close()
		return stale, errors.Join(stopErr, fmt.Errorf("%s: listen: %w", constant.TypeSamizdatPortal, addrFree(err)))
	}
	if portChange {
		stop()
	}

	newStale := p.setPairings(cfg.Pairings)
	p.cfg = cfg
	p.server = server
	p.ln = ln
	done := make(chan struct{})
	p.serveDone = done
	go func() {
		defer close(done)
		if err := server.Serve(ln); err != nil {
			p.logger.Error(constant.TypeSamizdatPortal, " stopped serving: ", addrFree(err))
		}
	}()
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
