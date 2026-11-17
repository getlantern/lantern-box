package group

import (
	"context"
	"net"
	"sync"
	"time"

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
// commits to First at First's first reply, and drops the copy. Before that, no
// reply within the commit timeout, a reset, or EOF moves it to Then: the leg
// to First is closed, Then is dialed, and the copy is replayed. A reply that
// races the move is discarded, since the replay will draw its own. Every
// transition happens under mu, so commit and move can't both win.
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
	overflow  bool  // the copy outgrew its bound, so the connection can't move
	moveErr   error // why the move to Then failed
	// moveCtx bounds the dial to Then; cancel ends it, on Close too.
	moveCtx context.Context
	cancel  context.CancelFunc
	// readDeadline and writeDeadline are reapplied to Then's leg on a move.
	readDeadline, writeDeadline time.Time
}

func (l *hedgeLeg) init(h *Hedge, ctx context.Context, key hedgeKey, network string, dest M.Socksaddr) {
	l.h, l.ctx, l.key, l.network, l.dest = h, ctx, key, network, dest
	l.onFirst = true
	l.moved = make(chan struct{})
	l.cancel = func() {}
}

// tentative reports whether the connection is on First and has not
// committed, so writes must be kept for replay. Caller holds mu.
func (l *hedgeLeg) tentativeLocked() bool {
	return l.onFirst && !l.committed
}

// awaitMove waits out a move in progress and reports whether the connection
// is now on Then, and the error if the move failed.
func (l *hedgeLeg) awaitMove() (onThen bool, err error) {
	l.mu.Lock()
	for l.moving {
		ch := l.moved
		l.mu.Unlock()
		<-ch
		l.mu.Lock()
	}
	defer l.mu.Unlock()
	return !l.onFirst, l.moveErr
}

// commit records First's reply. It reports false when a move already began,
// in which case the reply must be discarded.
func (l *hedgeLeg) commit() bool {
	l.mu.Lock()
	if l.moving || !l.onFirst {
		l.mu.Unlock()
		return false
	}
	first := !l.committed
	l.committed = true
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
// committed, moved, closed, or holds too much to replay. When the copy
// overflowed it still marks the site bad and closes First, so the client
// fails fast and retries on Then.
func (l *hedgeLeg) beginMove(why any, closeFirst func() error) bool {
	l.mu.Lock()
	if l.moving || !l.onFirst || l.committed || l.closed {
		l.mu.Unlock()
		return false
	}
	if l.overflow {
		l.mu.Unlock()
		l.h.markBad(l.ctx, l.key, why)
		_ = closeFirst()
		return false
	}
	l.moving = true
	l.moveCtx, l.cancel = failoverContext(l.ctx)
	l.mu.Unlock()
	l.h.markBad(l.ctx, l.key, why)
	_ = closeFirst()
	return true
}

// finishMove publishes the outcome of a move. Caller holds mu.
func (l *hedgeLeg) finishMoveLocked(err error) {
	l.moveErr = err
	l.onFirst = false
	l.moving = false
	l.cancel()
	close(l.moved)
}

// hedgeStream is a hedged TCP connection.
type hedgeStream struct {
	hedgeLeg
	cur  net.Conn
	held []byte
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

// move takes the connection to Then and replays what the client wrote.
func (s *hedgeStream) move(why lAdapter.UserFailureKind) {
	s.mu.Lock()
	old := s.cur
	s.mu.Unlock()
	if !s.beginMove(why, old.Close) {
		return
	}
	s.mu.Lock()
	ctx, held := s.moveCtx, s.held
	s.mu.Unlock()

	var conn net.Conn
	then, err := s.h.outbound(s.h.thenTag)
	if err == nil {
		conn, err = then.DialContext(ctx, s.network, s.dest)
	}
	if err == nil && len(held) > 0 {
		if _, err = conn.Write(held); err != nil {
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
		if !s.readDeadline.IsZero() {
			_ = conn.SetReadDeadline(s.readDeadline)
		}
		if !s.writeDeadline.IsZero() {
			_ = conn.SetWriteDeadline(s.writeDeadline)
		}
	}
	s.held = nil
	s.finishMoveLocked(err)
}

func (s *hedgeStream) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if s.moving {
			s.mu.Unlock()
			s.awaitMove()
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
			if s.commit() {
				return n, err
			}
			continue
		}
		if err == nil || isTimeout(err) {
			return n, err
		}
		s.move(lAdapter.UserFailureReset)
		if onThen, moveErr := s.awaitMove(); onThen {
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
			s.awaitMove()
			continue
		}
		if s.moveErr != nil {
			err := s.moveErr
			s.mu.Unlock()
			return 0, err
		}
		kept := false
		if s.tentativeLocked() && !s.overflow {
			if len(s.held)+len(p) <= s.h.cfg.maxHeldBytes {
				s.held = append(s.held, p...)
				kept = true
			} else {
				s.overflow = true
				s.held = nil
			}
		}
		c := s.cur
		s.mu.Unlock()

		n, err := c.Write(p)
		if err == nil || !kept {
			return n, err
		}
		// p is in the copy, so a move delivers it.
		s.move(lAdapter.UserFailureReset)
		if onThen, moveErr := s.awaitMove(); onThen {
			if moveErr != nil {
				return 0, moveErr
			}
			return len(p), nil
		}
		return n, err
	}
}

func (s *hedgeStream) Close() error {
	s.mu.Lock()
	s.closed = true
	c, cancel := s.cur, s.cancel
	s.mu.Unlock()
	cancel()
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
	s.mu.Lock()
	s.readDeadline, s.writeDeadline = t, t
	c := s.cur
	s.mu.Unlock()
	return c.SetDeadline(t)
}

func (s *hedgeStream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	c := s.cur
	s.mu.Unlock()
	return c.SetReadDeadline(t)
}

func (s *hedgeStream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.writeDeadline = t
	c := s.cur
	s.mu.Unlock()
	return c.SetWriteDeadline(t)
}

// hedgePacket is a hedged UDP connection. It keeps the datagrams the client
// sends until First replies, and replays them to Then on a move.
type hedgePacket struct {
	hedgeLeg
	cur       N.NetPacketConn
	held      []heldPacket
	heldBytes int
}

type heldPacket struct {
	data []byte
	dest M.Socksaddr
}

func newHedgePacket(h *Hedge, ctx context.Context, key hedgeKey, dest M.Socksaddr, first net.PacketConn) *hedgePacket {
	p := &hedgePacket{}
	p.init(h, ctx, key, N.NetworkUDP, dest)
	w := newDataPlanePacket(first, defaultDataPlaneIdle, defaultDataPlaneProvedReadBytes, dataPlaneHooks{
		onFailure: func(kind lAdapter.UserFailureKind) { p.firstFailed(kind, p.move) },
	})
	w.firstResponseTimeout = h.cfg.commitTimeout
	p.cur = w
	return p
}

func netPacketConn(c net.PacketConn) N.NetPacketConn {
	if npc, ok := c.(N.NetPacketConn); ok {
		return npc
	}
	return bufio.NewPacketConn(c)
}

func (p *hedgePacket) move(why lAdapter.UserFailureKind) {
	p.mu.Lock()
	old := p.cur
	p.mu.Unlock()
	if !p.beginMove(why, old.Close) {
		return
	}
	p.mu.Lock()
	ctx, held := p.moveCtx, p.held
	p.mu.Unlock()

	var conn N.NetPacketConn
	then, err := p.h.outbound(p.h.thenTag)
	if err == nil {
		var pc net.PacketConn
		if pc, err = then.ListenPacket(ctx, p.dest); err == nil {
			conn = netPacketConn(pc)
		}
	}
	for _, packet := range held {
		if err != nil {
			break
		}
		err = conn.WritePacket(buf.As(packet.data), packet.dest)
	}
	if err != nil && conn != nil {
		conn.Close()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err != nil:
	case p.closed:
		conn.Close()
		err = net.ErrClosed
	default:
		p.cur = conn
		if !p.readDeadline.IsZero() {
			_ = conn.SetReadDeadline(p.readDeadline)
		}
		if !p.writeDeadline.IsZero() {
			_ = conn.SetWriteDeadline(p.writeDeadline)
		}
	}
	p.held, p.heldBytes = nil, 0
	p.finishMoveLocked(err)
}

func (p *hedgePacket) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		if p.moving {
			p.mu.Unlock()
			p.awaitMove()
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
			if p.commit() {
				return n, addr, nil
			}
			continue
		}
		if isTimeout(err) {
			return n, addr, err
		}
		p.move(lAdapter.UserFailureReset)
		if onThen, moveErr := p.awaitMove(); onThen {
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
			p.awaitMove()
			continue
		}
		if p.moveErr != nil {
			err := p.moveErr
			p.mu.Unlock()
			return err
		}
		kept := false
		if p.tentativeLocked() && !p.overflow {
			if len(p.held) < hedgeMaxHeldPackets && p.heldBytes+len(b) <= p.h.cfg.maxHeldBytes {
				p.held = append(p.held, heldPacket{data: append([]byte(nil), b...), dest: dest})
				p.heldBytes += len(b)
				kept = true
			} else {
				p.overflow = true
				p.held, p.heldBytes = nil, 0
			}
		}
		c := p.cur
		p.mu.Unlock()

		err := c.WritePacket(buf.As(b), dest)
		if err == nil || !kept {
			return err
		}
		p.move(lAdapter.UserFailureReset)
		if onThen, moveErr := p.awaitMove(); onThen {
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
	p.closed = true
	c, cancel := p.cur, p.cancel
	p.mu.Unlock()
	cancel()
	return c.Close()
}

func (p *hedgePacket) LocalAddr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur.LocalAddr()
}

func (p *hedgePacket) SetDeadline(t time.Time) error {
	p.mu.Lock()
	p.readDeadline, p.writeDeadline = t, t
	c := p.cur
	p.mu.Unlock()
	return c.SetDeadline(t)
}

func (p *hedgePacket) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	p.readDeadline = t
	c := p.cur
	p.mu.Unlock()
	return c.SetReadDeadline(t)
}

func (p *hedgePacket) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	p.writeDeadline = t
	c := p.cur
	p.mu.Unlock()
	return c.SetWriteDeadline(t)
}

var (
	_ net.Conn        = (*hedgeStream)(nil)
	_ N.NetPacketConn = (*hedgePacket)(nil)
)
