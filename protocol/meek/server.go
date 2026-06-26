package meek

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ServerConfig configures a Server.
type ServerConfig struct {
	// Upstream is dialed for each new session and gets the bytes the
	// client posts; bytes from the upstream flow back as response
	// bodies. The server pipes bytes verbatim and is agnostic to the
	// upstream protocol, but the bundled meek outbound opens each session
	// with a SOCKS5 CONNECT, so a SOCKS5 proxy on the same host
	// (e.g. microsocks at "127.0.0.1:1080") is the expected deployment.
	Upstream string

	// MaxBodyBytes caps both the request-body the server accepts (larger
	// POSTs get 413) and the response-body it returns per POST. Default
	// 256 KiB (matches the client's default).
	MaxBodyBytes int

	// ResponseHoldoff is how long the server waits for upstream bytes
	// before responding with whatever it has (possibly empty).
	// Too small: many empty responses, idle CPU. Too large: high
	// client-perceived latency on the upstream-quiet path. Default
	// 50 ms.
	ResponseHoldoff time.Duration

	// SessionIdleTimeout is how long a session may go without a POST
	// before the reaper drops it. Should be at least 2-3x the client's
	// expected PollInterval to handle network blips. Default 5 min.
	SessionIdleTimeout time.Duration

	// Dialer optionally overrides net.Dial for upstream connections.
	Dialer func(network, address string) (net.Conn, error)

	// AuthToken, when non-empty, is a shared secret every request must
	// present in the X-Meek-Auth header. Without it the server is an
	// open relay into Upstream — anyone who reaches the endpoint can
	// create sessions and tunnel arbitrary traffic — so production
	// deployments on a public/fronted hostname MUST set it. Empty
	// disables the check (intended only for local tests).
	AuthToken string

	Logger *slog.Logger
}

func (c *ServerConfig) defaults() {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.ResponseHoldoff <= 0 {
		c.ResponseHoldoff = 50 * time.Millisecond
	}
	if c.SessionIdleTimeout <= 0 {
		c.SessionIdleTimeout = 5 * time.Minute
	}
	if c.Dialer == nil {
		c.Dialer = func(network, address string) (net.Conn, error) {
			return net.DialTimeout(network, address, 10*time.Second)
		}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server is an http.Handler implementing the meek-v1 protocol.
type Server struct {
	cfg ServerConfig

	mu       sync.Mutex
	sessions map[string]*session

	closeOnce sync.Once
	stop      chan struct{}
	reaperOK  chan struct{}
}

// NewServer constructs a Server and starts the session reaper goroutine.
// Call Close to stop the reaper and tear down all sessions.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Upstream == "" {
		return nil, errors.New("meek server: Upstream required")
	}
	cfg.defaults()
	s := &Server{
		cfg:      cfg,
		sessions: make(map[string]*session),
		stop:     make(chan struct{}),
		reaperOK: make(chan struct{}),
	}
	go s.reapLoop()
	return s, nil
}

// Close stops the reaper and closes every active upstream connection.
// Idempotent.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.reaperOK
		s.mu.Lock()
		for _, sess := range s.sessions {
			sess.close()
		}
		s.sessions = nil
		s.mu.Unlock()
	})
	return nil
}

// ServeHTTP handles a POST from a meek client. Non-POST requests get
// 405; missing X-Session-Id gets 400.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Constant-time compare so a probe can't time-discover the token.
	if s.cfg.AuthToken != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Meek-Auth")), []byte(s.cfg.AuthToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		http.Error(w, "missing X-Session-Id", http.StatusBadRequest)
		return
	}

	sess, isNew, err := s.getOrCreateSession(sid)
	if err != nil {
		s.cfg.Logger.Warn("meek server: upstream dial failed", slog.String("sid", sid), slog.Any("error", err))
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	if isNew {
		s.cfg.Logger.Debug("meek server: new session", slog.String("sid", sid))
	}

	// Read one byte past the cap so an oversized POST is rejected rather
	// than silently truncated: forwarding a truncated prefix upstream
	// would corrupt the tunneled TCP stream with no error to the client.
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxBodyBytes)+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > s.cfg.MaxBodyBytes {
		http.Error(w, "request body exceeds max_body_bytes", http.StatusRequestEntityTooLarge)
		return
	}

	// Response cap: honor the client's advertised read size (X-Meek-Max-Body),
	// bounded by our own limit. Clients that don't advertise are pre-negotiation
	// and read at most legacyMaxBodyBytes — never send them more, or they truncate.
	respCap := s.cfg.MaxBodyBytes
	if adv := parseMaxBody(r.Header.Get(headerMaxBody)); adv > 0 {
		if adv < respCap {
			respCap = adv
		}
	} else if respCap > legacyMaxBodyBytes {
		respCap = legacyMaxBodyBytes
	}

	seq, hasSeq := parseSeq(r.Header.Get(headerSeq))
	downstream, err := sess.serveRequest(hasSeq, seq, body, respCap, s.cfg.ResponseHoldoff)
	if err != nil {
		s.dropSession(sid)
		if errors.Is(err, errUpstreamClosed) {
			// Clean end-of-stream: upstream closed with nothing left. 410 tells the
			// client the session is gone so its Conn surfaces EOF instead of polling forever.
			s.cfg.Logger.Debug("meek server: upstream closed; ending session", slog.String("sid", sid))
			http.Error(w, "upstream closed", http.StatusGone)
		} else {
			s.cfg.Logger.Debug("meek server: upstream write failed; closing session", slog.String("sid", sid), slog.Any("error", err))
			http.Error(w, "upstream write", http.StatusBadGateway)
		}
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(downstream)))
	w.WriteHeader(http.StatusOK)
	if len(downstream) > 0 {
		_, _ = w.Write(downstream)
	}
}

// parseSeq parses an X-Meek-Seq header; ok is false when absent/invalid (legacy
// client — no dedupe). parseMaxBody parses X-Meek-Max-Body; 0 when absent.
func parseSeq(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseMaxBody(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func (s *Server) getOrCreateSession(sid string) (*session, bool, error) {
	s.mu.Lock()
	sess, ok := s.sessions[sid]
	if ok {
		sess.touch()
		s.mu.Unlock()
		return sess, false, nil
	}
	s.mu.Unlock()

	conn, err := s.cfg.Dialer("tcp", s.cfg.Upstream)
	if err != nil {
		return nil, false, fmt.Errorf("dial upstream %s: %w", s.cfg.Upstream, err)
	}
	sess = newSession(sid, conn)
	go sess.readPump(s.cfg.MaxBodyBytes * 4)

	s.mu.Lock()
	if existing, ok := s.sessions[sid]; ok {
		conn.Close()
		existing.touch()
		s.mu.Unlock()
		return existing, false, nil
	}
	s.sessions[sid] = sess
	s.mu.Unlock()
	return sess, true, nil
}

func (s *Server) dropSession(sid string) {
	s.mu.Lock()
	sess, ok := s.sessions[sid]
	if ok {
		delete(s.sessions, sid)
	}
	s.mu.Unlock()
	if ok {
		sess.close()
	}
}

func (s *Server) reapLoop() {
	defer close(s.reaperOK)
	t := time.NewTicker(s.cfg.SessionIdleTimeout / 2)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.reapOnce()
		}
	}
}

func (s *Server) reapOnce() {
	cutoff := time.Now().Add(-s.cfg.SessionIdleTimeout)
	s.mu.Lock()
	var dead []*session
	for sid, sess := range s.sessions {
		if sess.lastSeen().Before(cutoff) {
			dead = append(dead, sess)
			delete(s.sessions, sid)
		}
	}
	s.mu.Unlock()
	for _, sess := range dead {
		sess.close()
	}
}

// SessionCount is exposed for ops / metrics.
func (s *Server) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// --- session ---

type session struct {
	id       string
	upstream net.Conn

	mu           sync.Mutex
	pending      []byte
	closed       bool
	last         time.Time
	readWakeCh   chan struct{}
	upstreamDone chan struct{}
	// drainCond wakes a readPump paused because pending is at cap, when
	// takeLocked frees space or the session closes.
	drainCond *sync.Cond

	// reqMu serializes per-session request processing and guards the seq replay
	// state below. Separate from mu (which guards pending/readPump) so holding it
	// across writeUpstream+takeDownstream can't deadlock the read pump. lastResp
	// is the response sent for lastSeq, replayed verbatim if that seq is retried.
	reqMu    sync.Mutex
	haveSeq  bool
	lastSeq  uint64
	lastResp []byte
}

// errUpstreamClosed signals that the upstream is closed with nothing left to
// send, so ServeHTTP should drop the session and tell the client to tear down.
var errUpstreamClosed = errors.New("meek: upstream closed")

func newSession(id string, upstream net.Conn) *session {
	s := &session{
		id:           id,
		upstream:     upstream,
		last:         time.Now(),
		readWakeCh:   make(chan struct{}, 1),
		upstreamDone: make(chan struct{}),
	}
	s.drainCond = sync.NewCond(&s.mu)
	return s
}

// serveRequest applies a client poll to the session and returns the bytes to
// send back. With a sequence number it is idempotent: a repeated seq replays the
// buffered response without re-writing upstream or re-draining downstream, so a
// client that retries a lost poll neither duplicates upstream bytes nor drops
// downstream ones. Without a seq (pre-negotiation clients) every request is
// processed fresh — unchanged legacy behavior. reqMu serializes per-session
// requests so a retry that races the original simply waits and then replays.
func (s *session) serveRequest(hasSeq bool, seq uint64, body []byte, respCap int, holdoff time.Duration) ([]byte, error) {
	if !hasSeq {
		if len(body) > 0 {
			if err := s.writeUpstream(body); err != nil {
				return nil, err
			}
		}
		resp := s.takeDownstream(respCap, holdoff)
		if len(resp) == 0 && s.upstreamFinished() {
			return nil, errUpstreamClosed
		}
		return resp, nil
	}

	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	if s.haveSeq && seq == s.lastSeq {
		return s.lastResp, nil // retry of the last poll — replay its response
	}
	if len(body) > 0 {
		if err := s.writeUpstream(body); err != nil {
			return nil, err
		}
	}
	resp := s.takeDownstream(respCap, holdoff)
	if len(resp) == 0 && s.upstreamFinished() {
		return nil, errUpstreamClosed
	}
	s.haveSeq = true
	s.lastSeq = seq
	s.lastResp = resp
	return resp, nil
}

// upstreamFinished reports whether the upstream is closed AND all buffered
// downstream bytes have been drained — i.e. there is nothing left to ever send,
// so the session should end and the client tear down (otherwise a read-only
// client would poll forever on empty 200s, never seeing EOF).
func (s *session) upstreamFinished() bool {
	select {
	case <-s.upstreamDone:
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) == 0
}

func (s *session) lastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *session) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *session) writeUpstream(b []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	s.mu.Unlock()
	_, err := s.upstream.Write(b)
	return err
}

// readPump drains the upstream connection into pending until upstream
// closes or session.close is called. cap bounds how much we'll buffer
// — if pending fills, the pump pauses until takeDownstream drains it.
func (s *session) readPump(cap int) {
	defer close(s.upstreamDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.upstream.Read(buf)
		if n > 0 {
			s.mu.Lock()
			// Only block when there is already buffered data to drain.
			// With an empty buffer we append unconditionally so a single
			// read larger than cap (possible when MaxBodyBytes*4 < 32 KiB)
			// can't wedge the pump waiting for room that never frees.
			for len(s.pending) > 0 && len(s.pending)+n > cap && !s.closed {
				s.drainCond.Wait()
			}
			if s.closed {
				s.mu.Unlock()
				return
			}
			s.pending = append(s.pending, buf[:n]...)
			s.mu.Unlock()
			s.signalWake()
		}
		if err != nil {
			return
		}
	}
}

// takeDownstream returns up to max pending bytes. If pending is empty
// it blocks up to holdoff waiting for the readPump to deliver bytes,
// then returns whatever it has (possibly empty).
func (s *session) takeDownstream(max int, holdoff time.Duration) []byte {
	s.mu.Lock()
	if len(s.pending) > 0 {
		chunk := s.takeLocked(max)
		s.last = time.Now()
		s.mu.Unlock()
		return chunk
	}
	s.last = time.Now()
	s.mu.Unlock()

	select {
	case <-s.readWakeCh:
	case <-time.After(holdoff):
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeLocked(max)
}

func (s *session) takeLocked(max int) []byte {
	if len(s.pending) == 0 {
		return nil
	}
	n := len(s.pending)
	if n > max {
		n = max
	}
	out := make([]byte, n)
	copy(out, s.pending[:n])
	s.pending = s.pending[n:]
	// Wake a readPump paused at the cap.
	s.drainCond.Broadcast()
	return out
}

func (s *session) signalWake() {
	select {
	case s.readWakeCh <- struct{}{}:
	default:
	}
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.drainCond.Broadcast()
	s.mu.Unlock()
	s.upstream.Close()
	s.signalWake()
}
