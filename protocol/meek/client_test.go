package meek

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConn_RoundTrip(t *testing.T) {
	srv := newMeekTestServer()
	t.Cleanup(srv.Close)

	cfg := Config{
		URL:          srv.server.URL,
		HTTPClient:   srv.server.Client(),
		PollInterval: 20 * time.Millisecond,
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	buf := make([]byte, 32)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "HELLO" {
		t.Errorf("Read = %q; want %q", got, "HELLO")
	}
}

func TestConn_SessionPersistence(t *testing.T) {
	srv := newMeekTestServer()
	t.Cleanup(srv.Close)

	cfg := Config{URL: srv.server.URL, HTTPClient: srv.server.Client(), PollInterval: 20 * time.Millisecond}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	c.Write([]byte("a"))
	c.Write([]byte("bc"))
	time.Sleep(100 * time.Millisecond)
	c.Write([]byte("d"))
	time.Sleep(100 * time.Millisecond)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(srv.sessions))
	}
}

func TestConn_RequiresHTTPClient(t *testing.T) {
	_, err := Dial(context.Background(), Config{URL: "https://example.com/meek/"})
	if err == nil {
		t.Errorf("expected error when HTTPClient is nil")
	}
}

func TestConn_RequiresURL(t *testing.T) {
	_, err := Dial(context.Background(), Config{HTTPClient: http.DefaultClient})
	if err == nil {
		t.Errorf("expected error when URL is empty")
	}
}

// SetReadDeadline must unblock a parked Read when the deadline elapses
// in real time, not only when set in the past.
func TestConn_SetReadDeadlineUnblocksParkedRead(t *testing.T) {
	srv := newMeekTestServer()
	t.Cleanup(srv.Close)

	cfg := Config{URL: srv.server.URL, HTTPClient: srv.server.Client(), PollInterval: 50 * time.Millisecond}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// No Write — server has no upstream bytes, so Read parks immediately.
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	start := time.Now()
	buf := make([]byte, 4)
	_, err = c.Read(buf)
	elapsed := time.Since(start)

	if !errors.Is(err, errReadDeadline) {
		t.Errorf("Read err = %v; want errReadDeadline", err)
	}
	// Allow generous slack for CI scheduling jitter, but fail hard if
	// the Read either returned immediately (deadline not enforced at
	// all) or hung past 1s (timer didn't fire).
	if elapsed < 50*time.Millisecond {
		t.Errorf("Read returned too fast: %v", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("Read returned too slow: %v", elapsed)
	}
}

// A single Write larger than MaxWriteBufBytes must not buffer the whole
// payload at once: when the poll loop can't drain (front stalled), Write
// fills to the cap, blocks, and surfaces the write deadline having
// accepted far fewer than len(p) bytes. Guards the chunked-append cap.
func TestConn_LargeWriteRespectsBacklogCap(t *testing.T) {
	// A transport that parks every POST until the conn's context is
	// cancelled stalls the poll loop after at most one drained chunk, so
	// writeBuf can't be emptied. Using the request context (which is the
	// meek conn's ctx) means Close cancels it deterministically with no
	// leaked server goroutine.
	cfg := Config{
		URL: "https://meek.example/",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
		PollInterval:     2 * time.Second, // long, so only the Write signal drives the loop
		MaxBodyBytes:     512,
		MaxWriteBufBytes: 4096,
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const total = 1 << 20 // 1 MiB, far above the 4 KiB cap
	c.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := c.Write(make([]byte, total))
	if !errors.Is(err, errWriteDeadline) {
		t.Fatalf("Write err = %v; want errWriteDeadline", err)
	}
	// At most the cap plus the single drained chunk should have been
	// accepted — nowhere near the full payload.
	if n >= total {
		t.Errorf("Write accepted %d bytes; cap should have blocked well below %d", n, total)
	}
	if n > cfg.MaxWriteBufBytes+cfg.MaxBodyBytes {
		t.Errorf("Write accepted %d bytes; want <= cap+chunk (%d)", n, cfg.MaxWriteBufBytes+cfg.MaxBodyBytes)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// meekTestServer is a minimal meek server that uppercases every byte the
// client sends and queues it as response data on the next poll.
type meekTestServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	sessions map[string]*bytes.Buffer
}

// ExtraHeaders must not override protocol-critical headers: a config
// trying to pin X-Session-Id (which would collapse every conn onto one
// server-side session) is ignored, and the server sees the real
// per-conn random ID.
func TestConn_ReservedHeadersNotOverridable(t *testing.T) {
	srv := newMeekTestServer()
	t.Cleanup(srv.Close)

	cfg := Config{
		URL:          srv.server.URL,
		HTTPClient:   srv.server.Client(),
		PollInterval: 20 * time.Millisecond,
		ExtraHeaders: map[string]string{
			"X-Session-Id": "hijacked",
			"Content-Type": "text/plain",
			"X-Custom":     "ok", // non-reserved: should pass through
		},
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if _, hijacked := srv.sessions["hijacked"]; hijacked {
		t.Error("ExtraHeaders overrode X-Session-Id; reserved header not protected")
	}
	if _, ok := srv.sessions[c.sessionID]; !ok {
		t.Errorf("server never saw the real session id %q", c.sessionID)
	}
}

func newMeekTestServer() *meekTestServer {
	s := &meekTestServer{sessions: map[string]*bytes.Buffer{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *meekTestServer) Close() {
	s.server.Close()
}

func (s *meekTestServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sid := r.Header.Get("X-Session-Id")
	if sid == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	queue, ok := s.sessions[sid]
	if !ok {
		queue = &bytes.Buffer{}
		s.sessions[sid] = queue
	}
	for _, b := range body {
		if b >= 'a' && b <= 'z' {
			queue.WriteByte(b - 32)
		} else {
			queue.WriteByte(b)
		}
	}
	resp := queue.Bytes()
	queue.Reset()
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}
