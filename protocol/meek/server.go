package meek

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// ServerConfig configures a Server.
type ServerConfig struct {
	// Upstream is dialed for each new session and gets the bytes the
	// client posts; bytes from the upstream flow back as response
	// bodies. Typical deployment: a SOCKS5 inbound on the same host
	// (e.g. "127.0.0.1:1080") that handles destination selection.
	Upstream string

	// MaxBodyBytes caps the response-body length. The server returns
	// at most this many bytes per POST. Default 64 KiB.
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

	Logger *slog.Logger
}

func (c *ServerConfig) defaults() {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 64 * 1024
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

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.MaxBodyBytes)))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if len(body) > 0 {
		if err := sess.writeUpstream(body); err != nil {
			s.cfg.Logger.Debug("meek server: upstream write failed; closing session", slog.String("sid", sid), slog.Any("error", err))
			s.dropSession(sid)
			http.Error(w, "upstream write", http.StatusBadGateway)
			return
		}
	}

	downstream := sess.takeDownstream(s.cfg.MaxBodyBytes, s.cfg.ResponseHoldoff)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(downstream)))
	w.WriteHeader(http.StatusOK)
	if len(downstream) > 0 {
		_, _ = w.Write(downstream)
	}
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
}

func newSession(id string, upstream net.Conn) *session {
	return &session{
		id:           id,
		upstream:     upstream,
		last:         time.Now(),
		readWakeCh:   make(chan struct{}, 1),
		upstreamDone: make(chan struct{}),
	}
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
			for len(s.pending)+n > cap && !s.closed {
				s.mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				s.mu.Lock()
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
	s.mu.Unlock()
	s.upstream.Close()
	s.signalWake()
}
