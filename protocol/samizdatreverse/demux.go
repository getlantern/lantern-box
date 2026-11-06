package samizdatreverse

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/getlantern/lantern-box/constant"

	samizdat "github.com/getlantern/samizdat"
)

// helloTimeout bounds how long the portal waits for a connection's
// ClientHello before it closes the connection, as samizdat's own server
// does. A var so tests can shorten it.
var helloTimeout = 10 * time.Second

const (
	// masqueradeDialTimeout and masqueradeMaxDuration are samizdat's
	// defaults for its masquerade.
	masqueradeDialTimeout = 10 * time.Second
	masqueradeMaxDuration = 10 * time.Minute
)

// identity is one samizdat server identity the portal authenticates
// sessions under: an X25519 key and a certificate. Its samizdat server never
// accepts from a socket. The portal's listener reads each ClientHello and
// hands the connection to the identity whose key it authenticates under,
// through ln, and masquerades the rest itself, so the servers have no
// masquerade of their own.
//
// An identity the configuration no longer names is retired: it stops taking
// connections, but its live sessions go on until they end, and it closes
// once the last has.
type identity struct {
	privateKey, certPEM, keyPEM []byte
	publicKey                   []byte
	server                      *samizdat.Server
	ln                          *connListener
	serveDone                   chan struct{}

	mu        sync.Mutex
	live      int // sessions in the portal's handler
	retired   bool
	closed    bool
	closeOnce sync.Once
}

// newIdentity starts a samizdat server for an identity, accepting short IDs
// ids and handing sessions to p.handleSession.
func (p *Portal) newIdentity(privateKey, certPEM, keyPEM []byte, ids [][8]byte) (*identity, error) {
	priv, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	id := &identity{
		privateKey: privateKey,
		certPEM:    certPEM,
		keyPEM:     keyPEM,
		publicKey:  priv.PublicKey().Bytes(),
		ln:         newConnListener(),
		serveDone:  make(chan struct{}),
	}
	id.server, err = samizdat.NewServer(samizdat.ServerConfig{
		PrivateKey: privateKey,
		ShortIDs:   ids,
		CertPEM:    certPEM,
		KeyPEM:     keyPEM,
		SessionHandler: func(ctx context.Context, conn net.Conn, shortID [8]byte) {
			id.enter()
			defer id.leave()
			p.handleSession(ctx, conn, shortID)
		},
	})
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(id.serveDone)
		// Serve only returns once ln is closed.
		id.server.Serve(id.ln)
	}()
	return id, nil
}

// is reports whether id serves the given key and certificate.
func (id *identity) is(privateKey, certPEM, keyPEM []byte) bool {
	return bytes.Equal(id.privateKey, privateKey) &&
		bytes.Equal(id.certPEM, certPEM) &&
		bytes.Equal(id.keyPEM, keyPEM)
}

// authenticates reports whether sessionID carries a valid tag under id's key
// for one of its short IDs.
func (id *identity) authenticates(sessionID []byte) bool {
	_, ok, err := samizdat.VerifySessionID(sessionID, id.publicKey, id.server.ShortIDs())
	return err == nil && ok
}

func (id *identity) enter() {
	id.mu.Lock()
	id.live++
	id.mu.Unlock()
}

func (id *identity) leave() {
	id.mu.Lock()
	id.live--
	idle := id.retired && id.live == 0
	id.mu.Unlock()
	if idle {
		// Close waits for this session's handler, which is still running.
		go id.close()
	}
}

// retire stops id taking connections. It closes id at once if no session is
// live, and otherwise when the last one ends. It reports whether id is still
// open.
func (id *identity) retire() bool {
	id.mu.Lock()
	id.retired = true
	idle := id.live == 0
	id.mu.Unlock()
	id.ln.Close()
	if idle {
		id.close()
		return false
	}
	return true
}

// close stops id's server, which ends its sessions, and waits for it.
func (id *identity) close() {
	id.closeOnce.Do(func() {
		id.server.Close()
		<-id.serveDone
		id.mu.Lock()
		id.closed = true
		id.mu.Unlock()
	})
}

func (id *identity) isClosed() bool {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.closed
}

// closeIdentities closes ids in parallel, skipping nils.
func closeIdentities(ids ...*identity) {
	var wg sync.WaitGroup
	for _, id := range ids {
		if id == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			id.close()
		}()
	}
	wg.Wait()
}

// routes is what the portal's accept path reads to pick an identity. It is
// replaced whole, never changed.
type routes struct {
	cur  *identity
	prev *identity // nil when there is none
	// prevUntil is when prev stops taking new sessions.
	prevUntil time.Time
	// stop is done once the portal stops serving, which ends masquerades.
	// It lives as long as the identities do, across rebinds.
	stop   context.Context
	cancel context.CancelFunc
}

// pick returns the identity a ClientHello authenticates under: the current
// one, or the previous one while it is still accepting. It returns nil when
// the ClientHello authenticates under neither.
func (r *routes) pick(hello []byte, now time.Time) *identity {
	sessionID, err := samizdat.ExtractSessionID(hello)
	if err != nil {
		return nil
	}
	if r.cur.authenticates(sessionID) {
		return r.cur
	}
	if r.prev != nil && now.Before(r.prevUntil) && r.prev.authenticates(sessionID) {
		return r.prev
	}
	return nil
}

// startServe accepts on ln and routes each connection to an identity. p.mu
// must be held.
func (p *Portal) startServe(ln net.Listener) {
	p.ln = ln
	p.port = uint16(ln.Addr().(*net.TCPAddr).Port)
	done := make(chan struct{})
	p.serveDone = done
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				// net.ErrClosed means Rebind or a stop closed the listener.
				if !errors.Is(err, net.ErrClosed) {
					p.logger.Error(constant.TypeSamizdatPortal, " stopped serving: ", addrFree(err))
				}
				return
			}
			p.routeMu.Lock()
			p.routing[conn] = struct{}{}
			p.routeWG.Add(1)
			p.routeMu.Unlock()
			go p.route(conn)
		}
	}()
}

// route reads conn's ClientHello and hands conn, with the ClientHello to be
// read again, to the identity it authenticates under. A connection that
// authenticates under none is masqueraded, as samizdat's server would.
func (p *Portal) route(conn net.Conn) {
	defer p.routeWG.Done()
	// conn stays tracked, so a stop can close it, until route returns,
	// whichever way that is: a masqueraded conn for as long as it runs.
	defer func() {
		p.routeMu.Lock()
		delete(p.routing, conn)
		p.routeMu.Unlock()
	}()
	conn.SetReadDeadline(time.Now().Add(helloTimeout))
	record, err := readHelloRecord(conn)
	conn.SetReadDeadline(time.Time{})
	p.routeMu.Lock()
	// A stop that closed conn has also dropped it, and it must not be
	// handed on.
	_, tracked := p.routing[conn]
	p.routeMu.Unlock()
	r := p.routes.Load()
	if err != nil || !tracked || r == nil {
		conn.Close()
		return
	}
	if id := r.pick(record[5:], time.Now()); id != nil {
		if !id.ln.deliver(&replayConn{Conn: conn, buf: record}) {
			conn.Close()
		}
		return
	}
	p.masquerade(r.stop, conn, record)
}

// masquerade relays conn to the masquerade origin, starting with the
// ClientHello record it has already read, as samizdat's server does with a
// connection that fails to authenticate. It closes conn when the relay ends,
// at once with no masquerade configured, and when stop is done. It works on
// the accepted conn itself, so a half-close from the origin reaches the
// client.
func (p *Portal) masquerade(stop context.Context, conn net.Conn, record []byte) {
	defer conn.Close()
	if p.masqueradeDomain == "" {
		return
	}
	addr := p.masqueradeAddr
	if addr == "" {
		addr = net.JoinHostPort(p.masqueradeDomain, "443")
	}
	origin, err := (&net.Dialer{Timeout: masqueradeDialTimeout}).DialContext(stop, "tcp", addr)
	if err != nil {
		return
	}
	defer origin.Close()
	unhook := context.AfterFunc(stop, func() {
		origin.Close()
		conn.Close()
	})
	defer unhook()
	if _, err := origin.Write(record); err != nil {
		return
	}
	deadline := time.Now().Add(masqueradeMaxDuration)
	conn.SetDeadline(deadline)
	origin.SetDeadline(deadline)
	var wg sync.WaitGroup
	wg.Add(2)
	relay := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}
	go relay(origin, conn)
	go relay(conn, origin)
	wg.Wait()
}

// stopRoutingLocked closes the connections still waiting for a ClientHello
// or being masqueraded and waits for every route call to finish. The accept
// loop must have stopped. p.mu must be held.
func (p *Portal) stopRoutingLocked() {
	p.routes.Load().cancel()
	p.routeMu.Lock()
	for conn := range p.routing {
		conn.Close()
		delete(p.routing, conn)
	}
	p.routeMu.Unlock()
	p.routeWG.Wait()
}

// readHelloRecord reads one TLS handshake record, which holds the
// ClientHello, and returns it whole, header included.
func readHelloRecord(conn net.Conn) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	if header[0] != 22 {
		return nil, errors.New("not a TLS handshake record")
	}
	n := int(header[3])<<8 | int(header[4])
	if n > 16384 {
		return nil, errors.New("TLS record too large")
	}
	record := make([]byte, 5+n)
	copy(record, header)
	if _, err := io.ReadFull(conn, record[5:]); err != nil {
		return nil, err
	}
	return record, nil
}

// replayConn returns buf to its first reads, then reads from Conn.
type replayConn struct {
	net.Conn
	buf []byte
}

func (c *replayConn) Read(b []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// connListener is a net.Listener whose connections are handed to it by
// deliver.
type connListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newConnListener() *connListener {
	return &connListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

// deliver hands conn to Accept. It reports false if the listener is closed,
// and conn is then the caller's to close.
func (l *connListener) deliver(conn net.Conn) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.conns <- conn:
		return true
	case <-l.done:
		return false
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// Addr returns a placeholder: the listener has no address of its own.
func (l *connListener) Addr() net.Addr { return &net.TCPAddr{} }
