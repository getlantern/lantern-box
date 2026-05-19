package meek

import (
	"bytes"
	"context"
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

// meekTestServer is a minimal meek server that uppercases every byte the
// client sends and queues it as response data on the next poll.
type meekTestServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	sessions map[string]*bytes.Buffer
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
