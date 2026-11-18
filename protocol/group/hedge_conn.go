package group

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

// hedgeLeg is the state a hedged connection shares between its stream and
// packet forms: which leg it is on and whether it may still move.
//
// A connection starts on First, holding a copy of what the client writes. It
// commits to First at First's first reply, and the copy is released. Before
// that, no reply within the commit timeout, a reset, or EOF moves it to Then:
// the leg to First is closed, and Then is dialed and given the copy in the
// background, while any caller waiting on the move is bound by its own
// deadline. A reply that races the move is discarded, since the replay will
// draw its own. Every transition happens under mu, so commit and move can't
// both win; I/O runs with mu released.
//
// Only a connection that opens with a handshake is replayed: a TLS record on
// TCP, a QUIC long-header packet on UDP. Repeating a ClientHello or an
// Initial makes the server start a fresh handshake, while repeating anything
// else, such as a plaintext request, could act on it twice. Any other
// connection stays on First: it is never moved, a slow reply is left to the
// client's own timeouts, and a reset only marks the site bad.
type hedgeLeg struct {
	h       *Hedge
	ctx     context.Context
	key     hedgeKey
	network string
	dest    M.Socksaddr

	mu        sync.Mutex
	onFirst   bool
	committed bool
	moving    bool
	moved     chan struct{} // closed when a move finishes
	closed    bool
	// decided is set at the first non-empty write, which tells whether the
	// connection can be replayed. noReplay is set when it can't, and
	// overflow when the copy outgrew its bound.
	decided  bool
	noReplay bool
	overflow bool
	moveErr  error // why the move to Then failed
	// moveCtx bounds the dial to Then; cancel ends it, on Close too.
	moveCtx context.Context
	cancel  context.CancelFunc
	// pending is Then's leg while the copy is replayed to it, so Close can
	// interrupt the replay and deadline changes reach it.
	pending deadlineCloser
	// readDeadline and writeDeadline bound waits for a move and are
	// applied to Then's leg. deadlineChanged is closed and replaced whenever
	// either changes, waking waiters to re-read them.
	readDeadline, writeDeadline time.Time
	deadlineChanged             chan struct{}
}

type deadlineCloser interface {
	io.Closer
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func (l *hedgeLeg) init(h *Hedge, ctx context.Context, key hedgeKey, network string, dest M.Socksaddr) {
	l.h, l.ctx, l.key, l.network, l.dest = h, ctx, key, network, dest
	l.onFirst = true
	l.moved = make(chan struct{})
	l.deadlineChanged = make(chan struct{})
	l.cancel = func() {}
}

// tentativeLocked reports whether the connection is on First and has not
// committed, so writes must be kept for replay. Caller holds mu.
func (l *hedgeLeg) tentativeLocked() bool {
	return l.onFirst && !l.committed
}

// keepLocked decides whether b, about to be written, is kept for replay. The
// first non-empty write decides whether the connection is replayable at all;
// an empty write, which the router uses to kick a deferred handshake, decides
// nothing. Caller holds mu.
func (l *hedgeLeg) keepLocked(b []byte, opensHandshake func([]byte) bool, fits bool) bool {
	if len(b) == 0 || !l.tentativeLocked() || l.noReplay || l.overflow {
		return false
	}
	if !l.decided {
		l.decided = true
		if !opensHandshake(b) {
			l.noReplay = true
			return false
		}
	}
	if !fits {
		l.overflow = true
		return false
	}
	return true
}

// awaitMove waits out a move in progress. It reports whether the connection
// is now on Then and the error if the move failed, or os.ErrDeadlineExceeded
// when the deadline passes first. The deadline is re-read whenever it
// changes; a zero deadline waits for the move.
func (l *hedgeLeg) awaitMove(deadline func() time.Time) (onThen bool, err error) {
	l.mu.Lock()
	for l.moving {
		moved, changed, dl := l.moved, l.deadlineChanged, deadline()
		l.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			timer = time.NewTimer(time.Until(dl))
			expired = timer.C
		}
		select {
		case <-moved:
		case <-changed:
		case <-expired:
			return false, os.ErrDeadlineExceeded
		}
		if timer != nil {
			timer.Stop()
		}
		l.mu.Lock()
	}
	defer l.mu.Unlock()
	return !l.onFirst, l.moveErr
}

func (l *hedgeLeg) readDeadlineLocked() time.Time  { return l.readDeadline }
func (l *hedgeLeg) writeDeadlineLocked() time.Time { return l.writeDeadline }

// setDeadlines records new deadlines and wakes waiters. During a move it
// applies them to Then's pending leg, since First is closed, and reports
// moving so the caller skips First. The replay stays bound by the failover
// timeout whatever the caller sets.
func (l *hedgeLeg) setDeadlines(read, write *time.Time) (moving bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if read != nil {
		l.readDeadline = *read
	}
	if write != nil {
		l.writeDeadline = *write
	}
	close(l.deadlineChanged)
	l.deadlineChanged = make(chan struct{})
	if !l.moving {
		return false
	}
	if l.pending != nil {
		if read != nil {
			_ = l.pending.SetReadDeadline(*read)
		}
		if write != nil {
			_ = l.pending.SetWriteDeadline(*write)
		}
	}
	return true
}

// commit records First's reply and runs release under mu to free the copy.
// It reports false when a move already began, in which case the reply must
// be discarded.
func (l *hedgeLeg) commit(release func()) bool {
	l.mu.Lock()
	if l.moving || !l.onFirst {
		l.mu.Unlock()
		return false
	}
	first := !l.committed
	l.committed = true
	release()
	l.mu.Unlock()
	if first {
		l.h.markGood(l.key)
	}
	return true
}

// firstFailed handles a failure the watchdog saw on First: before a commit it
// moves the connection, after one it only marks the site bad, since the
// client has already seen First's bytes.
func (l *hedgeLeg) firstFailed(kind lAdapter.UserFailureKind, move func(lAdapter.UserFailureKind)) {
	l.mu.Lock()
	committed := l.committed
	l.mu.Unlock()
	if committed {
		l.h.markBad(l.ctx, l.key, kind)
		return
	}
	move(kind)
}

// beginMove claims the move. It returns false when the connection already
// committed, moved, or closed, or can't be replayed, and then the connection
// stays on First. One that doesn't open with a handshake is left alone when
// First is merely slow. Otherwise, and for one whose copy overflowed, the
// failure marks the site bad and closes First, so the client fails fast and
// retries on Then.
func (l *hedgeLeg) beginMove(why lAdapter.UserFailureKind, closeFirst func() error) bool {
	l.mu.Lock()
	if l.moving || !l.onFirst || l.committed || l.closed {
		l.mu.Unlock()
		return false
	}
	if l.noReplay || l.overflow {
		slowOnly := l.noReplay && why == lAdapter.UserFailureNoResponse
		l.mu.Unlock()
		if !slowOnly {
			l.h.markBad(l.ctx, l.key, why)
			_ = closeFirst()
		}
		return false
	}
	l.moving = true
	l.moveCtx, l.cancel = failoverContext(l.ctx)
	l.mu.Unlock()
	l.h.markBad(l.ctx, l.key, why)
	_ = closeFirst()
	return true
}

// startReplay records Then's leg so Close and deadline changes reach it, and
// applies the caller's current deadlines to it. It reports false when the
// connection closed meanwhile.
func (l *hedgeLeg) startReplay(c deadlineCloser) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.pending = c
	_ = c.SetReadDeadline(l.readDeadline)
	_ = c.SetWriteDeadline(l.writeDeadline)
	return true
}

// replayTimer closes c if the replay outlives the failover timeout, which
// holds even for a conn that ignores deadlines. done reports whether the
// replay finished first.
func replayTimer(c io.Closer) (done func() bool) {
	timer := time.AfterFunc(hedgeFailoverDialTimeout, func() { _ = c.Close() })
	return timer.Stop
}

// finishMoveLocked publishes the outcome of a move. Caller holds mu.
func (l *hedgeLeg) finishMoveLocked(err error) {
	l.moveErr = err
	l.onFirst = false
	l.moving = false
	l.pending = nil
	l.cancel()
	close(l.moved)
}

// closeLocked marks the connection closed and returns what must be closed
// with it. Caller holds mu.
func (l *hedgeLeg) closeLocked() (cancel context.CancelFunc, pending io.Closer) {
	l.closed = true
	if l.pending == nil {
		return l.cancel, nil
	}
	return l.cancel, l.pending
}

// opensTLS reports whether b starts with a TLS handshake record.
func opensTLS(b []byte) bool {
	return len(b) >= 3 && b[0] == 0x16 && b[1] == 0x03
}

// opensQUIC reports whether b is a QUIC long-header packet, the form every
// handshake packet takes in versions 1 and 2.
func opensQUIC(b []byte) bool {
	return len(b) >= 5 && b[0]&0xc0 == 0xc0
}

// hedgeStream is a hedged TCP connection.
type hedgeStream struct {
	hedgeLeg
	cur  net.Conn
	held []byte
	// writeClosed records the client's half-close, to repeat on Then's leg.
	writeClosed bool
}

func newHedgeStream(h *Hedge, ctx context.Context, key hedgeKey, network string, dest M.Socksaddr, first net.Conn) *hedgeStream {
	s := &hedgeStream{}
	s.init(h, ctx, key, network, dest)
	w := newDataPlaneStream(first, defaultDataPlaneIdle, defaultDataPlaneProvedReadBytes, dataPlaneHooks{
		onFailure: func(kind lAdapter.UserFailureKind) { s.firstFailed(kind, s.move) },
	})
	w.firstResponseTimeout = h.cfg.commitTimeout
	s.cur = w
	return s
}

// move claims the move and runs it in the background, so whoever triggered
// it waits through awaitMove, bound by its own deadline.
func (s *hedgeStream) move(why lAdapter.UserFailureKind) {
	s.mu.Lock()
	old := s.cur
	s.mu.Unlock()
	if s.beginMove(why, old.Close) {
		go s.runMove()
	}
}

// runMove dials Then and replays what the client wrote.
func (s *hedgeStream) runMove() {
	s.mu.Lock()
	ctx, held := s.moveCtx, s.held
	s.mu.Unlock()

	var conn net.Conn
	then, err := s.h.outbound(s.h.thenTag)
	if err == nil {
		conn, err = then.DialContext(ctx, s.network, s.dest)
	}
	if err == nil {
		switch {
		case !s.startReplay(conn):
			err = net.ErrClosed
		case len(held) > 0:
			done := replayTimer(conn)
			_, err = conn.Write(held)
			if !done() && err == nil {
				err = os.ErrDeadlineExceeded
			}
		}
		if err != nil {
			conn.Close()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
	case s.closed:
		conn.Close()
		err = net.ErrClosed
	default:
		s.cur = conn
		if s.writeClosed {
			_ = closeWrite(conn)
		}
	}
	s.held = nil
	s.finishMoveLocked(err)
}

func (s *hedgeStream) releaseLocked() { s.held = nil }

// closeWrite half-closes c, or reports errors.ErrUnsupported when it can't,
// so the caller closes it instead.
func closeWrite(c net.Conn) error {
	if wc, ok := common.Cast[N.WriteCloser](c); ok {
		return wc.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (s *hedgeStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if s.moving {
			s.mu.Unlock()
			if _, err := s.awaitMove(s.readDeadlineLocked); err == os.ErrDeadlineExceeded {
				return 0, err
			}
			continue
		}
		if s.moveErr != nil {
			err := s.moveErr
			s.mu.Unlock()
			return 0, err
		}
		c, tentative := s.cur, s.tentativeLocked()
		s.mu.Unlock()

		n, err := c.Read(p)
		if !tentative {
			return n, err
		}
		if n > 0 {
			if s.commit(s.releaseLocked) {
				return n, err
			}
			continue
		}
		if err == nil || isTimeout(err) {
			return n, err
		}
		s.move(lAdapter.UserFailureReset)
		onThen, moveErr := s.awaitMove(s.readDeadlineLocked)
		if moveErr == os.ErrDeadlineExceeded {
			return 0, moveErr
		}
		if onThen {
			if moveErr != nil {
				return 0, moveErr
			}
			continue
		}
		return n, err
	}
}

func (s *hedgeStream) Write(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, net.ErrClosed
		}
		if s.moving {
			s.mu.Unlock()
			if _, err := s.awaitMove(s.writeDeadlineLocked); err == os.ErrDeadlineExceeded {
				return 0, err
			}
			continue
		}
		if s.moveErr != nil {
			err := s.moveErr
			s.mu.Unlock()
			return 0, err
		}
		kept := s.keepLocked(p, opensTLS, len(s.held)+len(p) <= s.h.cfg.maxHeldBytes)
		if kept {
			s.held = append(s.held, p...)
		} else if s.overflow {
			s.held = nil
		}
		c := s.cur
		s.mu.Unlock()

		n, err := c.Write(p)
		if err == nil || !kept {
			return n, err
		}
		// p is in the copy, so a move delivers it.
		s.move(lAdapter.UserFailureReset)
		onThen, moveErr := s.awaitMove(s.writeDeadlineLocked)
		if moveErr == os.ErrDeadlineExceeded {
			return 0, moveErr
		}
		if onThen {
			if moveErr != nil {
				return 0, moveErr
			}
			return len(p), nil
		}
		return n, err
	}
}

// CloseWrite half-closes the current leg, and records it so a later move
// half-closes Then's leg after the replay. A leg that can't half-close
// reports errors.ErrUnsupported, and the caller closes the connection.
func (s *hedgeStream) CloseWrite() error {
	for {
		s.mu.Lock()
		if s.moving {
			s.mu.Unlock()
			if _, err := s.awaitMove(s.writeDeadlineLocked); err == os.ErrDeadlineExceeded {
				return err
			}
			continue
		}
		if s.moveErr != nil {
			err := s.moveErr
			s.mu.Unlock()
			return err
		}
		s.writeClosed = true
		c := s.cur
		s.mu.Unlock()
		return closeWrite(c)
	}
}

// NeedHandshakeForWrite reports whether the current leg waits for a first
// write, such as a client-info frame, so the router can kick it with an
// empty write for server-first protocols.
func (s *hedgeStream) NeedHandshakeForWrite() bool {
	s.mu.Lock()
	c, moving := s.cur, s.moving
	s.mu.Unlock()
	return !moving && N.NeedHandshakeForWrite(c)
}

func (s *hedgeStream) Close() error {
	s.mu.Lock()
	cancel, pending := s.closeLocked()
	c := s.cur
	s.mu.Unlock()
	cancel()
	if pending != nil {
		_ = pending.Close()
	}
	return c.Close()
}

func (s *hedgeStream) LocalAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.LocalAddr()
}

func (s *hedgeStream) RemoteAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.RemoteAddr()
}

func (s *hedgeStream) SetDeadline(t time.Time) error {
	if s.setDeadlines(&t, &t) {
		return nil
	}
	return s.current().SetDeadline(t)
}

func (s *hedgeStream) SetReadDeadline(t time.Time) error {
	if s.setDeadlines(&t, nil) {
		return nil
	}
	return s.current().SetReadDeadline(t)
}

func (s *hedgeStream) SetWriteDeadline(t time.Time) error {
	if s.setDeadlines(nil, &t) {
		return nil
	}
	return s.current().SetWriteDeadline(t)
}

func (s *hedgeStream) current() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// hedgePacket is a hedged UDP connection. It keeps the datagrams the client
// sends until First replies, and replays them to Then on a move.
type hedgePacket struct {
	hedgeLeg
	cur N.NetPacketConn
	// curRaw is cur before any adapter, for headroom lookups.
	curRaw    net.PacketConn
	held      []heldPacket
	heldBytes int
	// cids are the source connection IDs of the client's QUIC long-header
	// packets, the only ones a Version Negotiation addressed to the client
	// can carry.
	cids [][]byte
}

type heldPacket struct {
	data []byte
	dest M.Socksaddr
}

// hedgeMaxCIDs bounds the connection IDs remembered per connection; a QUIC
// client uses one or two before its handshake completes.
const hedgeMaxCIDs = 8

func newHedgePacket(h *Hedge, ctx context.Context, key hedgeKey, dest M.Socksaddr, first net.PacketConn) *hedgePacket {
	p := &hedgePacket{}
	p.init(h, ctx, key, N.NetworkUDP, dest)
	// The filter sits under the watchdog, so a decoy's Version Negotiation
	// neither commits the connection nor counts as First's reply.
	filtered := &vnFilter{PacketConn: first, ours: p.ownsCID}
	w := newDataPlanePacket(filtered, defaultDataPlaneIdle, defaultDataPlaneProvedReadBytes, dataPlaneHooks{
		onFailure: func(kind lAdapter.UserFailureKind) { p.firstFailed(kind, p.move) },
	})
	w.firstResponseTimeout = h.cfg.commitTimeout
	p.cur, p.curRaw = w, w
	return p
}

// vnFilter drops QUIC Version Negotiation packets that don't answer the
// client. A decoy prelude, such as quicprelude's, draws a Version
// Negotiation from servers that don't speak its version; it is addressed to
// the decoy's connection ID and tells nothing about whether the client's own
// handshake got through.
type vnFilter struct {
	net.PacketConn
	ours func(cid []byte) bool
}

func (f *vnFilter) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := f.PacketConn.ReadFrom(b)
		if err == nil {
			if cid, ok := versionNegotiationCID(b[:n]); ok && !f.ours(cid) {
				continue
			}
		}
		return n, addr, err
	}
}

// ReadPacket reads through ReadFrom, so the filter applies.
func (f *vnFilter) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(f)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

// WritePacket keeps domain-name destinations for a conn that resolves them;
// WriteTo's net.Addr would strip them to an IP.
func (f *vnFilter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	return netPacketConn(f.PacketConn).WritePacket(buffer, destination)
}

func (f *vnFilter) Upstream() any { return f.PacketConn }

// versionNegotiationCID returns the destination connection ID of a QUIC
// Version Negotiation packet: a long header whose version is zero.
func versionNegotiationCID(b []byte) ([]byte, bool) {
	if len(b) < 6 || b[0]&0x80 == 0 || b[1]|b[2]|b[3]|b[4] != 0 {
		return nil, false
	}
	l := int(b[5])
	if len(b) < 6+l {
		return nil, false
	}
	return b[6 : 6+l], true
}

// sourceCID returns the source connection ID of a QUIC long-header packet.
func sourceCID(b []byte) ([]byte, bool) {
	if len(b) < 6 || b[0]&0x80 == 0 {
		return nil, false
	}
	at := 6 + int(b[5])
	if len(b) <= at {
		return nil, false
	}
	l := int(b[at])
	if len(b) < at+1+l {
		return nil, false
	}
	return b[at+1 : at+1+l], true
}

// noteCIDLocked remembers the source connection ID of a long-header packet
// the client sends. Caller holds mu.
func (p *hedgePacket) noteCIDLocked(b []byte) {
	cid, ok := sourceCID(b)
	if !ok || len(p.cids) >= hedgeMaxCIDs {
		return
	}
	for _, known := range p.cids {
		if bytes.Equal(known, cid) {
			return
		}
	}
	p.cids = append(p.cids, append([]byte(nil), cid...))
}

func (p *hedgePacket) ownsCID(cid []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, known := range p.cids {
		if bytes.Equal(known, cid) {
			return true
		}
	}
	return false
}

func netPacketConn(c net.PacketConn) N.NetPacketConn {
	if npc, ok := c.(N.NetPacketConn); ok {
		return npc
	}
	return bufio.NewPacketConn(c)
}

// writePacket writes b to conn in a buffer with the headroom conn needs.
// Packet conns that frame datagrams, such as SOCKS5 and Shadowsocks, write
// their header in front of the payload and panic without room for it.
func writePacket(conn N.NetPacketConn, raw net.PacketConn, b []byte, dest M.Socksaddr) error {
	front, rear := N.CalculateFrontHeadroom(raw), N.CalculateRearHeadroom(raw)
	buffer := buf.NewSize(front + len(b) + rear)
	buffer.Resize(front, 0)
	_, _ = buffer.Write(b)
	return conn.WritePacket(buffer, dest)
}

func (p *hedgePacket) move(why lAdapter.UserFailureKind) {
	p.mu.Lock()
	old := p.cur
	p.mu.Unlock()
	if p.beginMove(why, old.Close) {
		go p.runMove()
	}
}

func (p *hedgePacket) runMove() {
	p.mu.Lock()
	ctx, held := p.moveCtx, p.held
	p.mu.Unlock()

	var (
		conn N.NetPacketConn
		raw  net.PacketConn
	)
	then, err := p.h.outbound(p.h.thenTag)
	if err == nil {
		if raw, err = then.ListenPacket(ctx, p.dest); err == nil {
			conn = netPacketConn(raw)
		}
	}
	if err == nil {
		if !p.startReplay(conn) {
			err = net.ErrClosed
		} else if len(held) > 0 {
			done := replayTimer(conn)
			for _, packet := range held {
				if err = writePacket(conn, raw, packet.data, packet.dest); err != nil {
					break
				}
			}
			if !done() && err == nil {
				err = os.ErrDeadlineExceeded
			}
		}
		if err != nil {
			conn.Close()
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err != nil:
	case p.closed:
		conn.Close()
		err = net.ErrClosed
	default:
		p.cur, p.curRaw = conn, raw
	}
	p.held, p.heldBytes = nil, 0
	p.finishMoveLocked(err)
}

func (p *hedgePacket) releaseLocked() { p.held, p.heldBytes = nil, 0 }

func (p *hedgePacket) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		if p.moving {
			p.mu.Unlock()
			if _, err := p.awaitMove(p.readDeadlineLocked); err == os.ErrDeadlineExceeded {
				return 0, nil, err
			}
			continue
		}
		if p.moveErr != nil {
			err := p.moveErr
			p.mu.Unlock()
			return 0, nil, err
		}
		c, tentative := p.cur, p.tentativeLocked()
		p.mu.Unlock()

		n, addr, err := c.ReadFrom(b)
		if !tentative {
			return n, addr, err
		}
		if err == nil {
			if p.commit(p.releaseLocked) {
				return n, addr, nil
			}
			continue
		}
		if isTimeout(err) {
			return n, addr, err
		}
		p.move(lAdapter.UserFailureReset)
		onThen, moveErr := p.awaitMove(p.readDeadlineLocked)
		if moveErr == os.ErrDeadlineExceeded {
			return 0, nil, moveErr
		}
		if onThen {
			if moveErr != nil {
				return 0, nil, moveErr
			}
			continue
		}
		return n, addr, err
	}
}

func (p *hedgePacket) write(b []byte, dest M.Socksaddr) error {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return net.ErrClosed
		}
		if p.moving {
			p.mu.Unlock()
			if _, err := p.awaitMove(p.writeDeadlineLocked); err == os.ErrDeadlineExceeded {
				return err
			}
			continue
		}
		if p.moveErr != nil {
			err := p.moveErr
			p.mu.Unlock()
			return err
		}
		if p.tentativeLocked() {
			p.noteCIDLocked(b)
		}
		fits := len(p.held) < hedgeMaxHeldPackets && p.heldBytes+len(b) <= p.h.cfg.maxHeldBytes
		kept := p.keepLocked(b, opensQUIC, fits)
		if kept {
			p.held = append(p.held, heldPacket{data: append([]byte(nil), b...), dest: dest})
			p.heldBytes += len(b)
		} else if p.overflow {
			p.held, p.heldBytes = nil, 0
		}
		c, raw := p.cur, p.curRaw
		p.mu.Unlock()

		err := writePacket(c, raw, b, dest)
		if err == nil || !kept {
			return err
		}
		p.move(lAdapter.UserFailureReset)
		onThen, moveErr := p.awaitMove(p.writeDeadlineLocked)
		if moveErr == os.ErrDeadlineExceeded || onThen {
			return moveErr
		}
		return err
	}
}

func (p *hedgePacket) WriteTo(b []byte, addr net.Addr) (int, error) {
	if err := p.write(b, M.SocksaddrFromNet(addr)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// ReadPacket reads through ReadFrom, so a reply can commit or be discarded.
func (p *hedgePacket) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(p)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

// WritePacket keeps domain-name destinations, which WriteTo's net.Addr would
// strip to an IP.
func (p *hedgePacket) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()
	return p.write(buffer.Bytes(), destination)
}

func (p *hedgePacket) Close() error {
	p.mu.Lock()
	cancel, pending := p.closeLocked()
	c := p.cur
	p.mu.Unlock()
	cancel()
	if pending != nil {
		_ = pending.Close()
	}
	return c.Close()
}

func (p *hedgePacket) LocalAddr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur.LocalAddr()
}

func (p *hedgePacket) SetDeadline(t time.Time) error {
	if p.setDeadlines(&t, &t) {
		return nil
	}
	return p.current().SetDeadline(t)
}

func (p *hedgePacket) SetReadDeadline(t time.Time) error {
	if p.setDeadlines(&t, nil) {
		return nil
	}
	return p.current().SetReadDeadline(t)
}

func (p *hedgePacket) SetWriteDeadline(t time.Time) error {
	if p.setDeadlines(nil, &t) {
		return nil
	}
	return p.current().SetWriteDeadline(t)
}

func (p *hedgePacket) current() N.NetPacketConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

var (
	_ net.Conn        = (*hedgeStream)(nil)
	_ N.WriteCloser   = (*hedgeStream)(nil)
	_ N.EarlyWriter   = (*hedgeStream)(nil)
	_ N.NetPacketConn = (*hedgePacket)(nil)
	_ N.NetPacketConn = (*vnFilter)(nil)
)
