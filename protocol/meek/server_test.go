package meek

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServer_EndToEndEcho(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)

	srv, err := NewServer(ServerConfig{
		Upstream:           upstream.addr,
		ResponseHoldoff:    30 * time.Millisecond,
		SessionIdleTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	cfg := Config{
		URL:          hs.URL,
		HTTPClient:   hs.Client(),
		PollInterval: 20 * time.Millisecond,
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Write([]byte("hello over meek")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := make([]byte, 64)
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, err := c.Read(got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s := string(got[:n]); s != "hello over meek" {
		t.Errorf("got %q; want %q", s, "hello over meek")
	}

	if got := srv.SessionCount(); got != 1 {
		t.Errorf("SessionCount = %d; want 1", got)
	}
}

func TestServer_LargeBidirectional(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)

	srv, err := NewServer(ServerConfig{
		Upstream:           upstream.addr,
		ResponseHoldoff:    20 * time.Millisecond,
		SessionIdleTimeout: 2 * time.Second,
		MaxBodyBytes:       4096,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	cfg := Config{
		URL:          hs.URL,
		HTTPClient:   hs.Client(),
		PollInterval: 10 * time.Millisecond,
		MaxBodyBytes: 4096,
	}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const payload = "abcdefghijklmnopqrstuvwxyz0123456789"
	send := strings.Repeat(payload, 1000) // 36 KB
	go func() {
		_, _ = c.Write([]byte(send))
	}()

	var recv []byte
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8*1024)
	for len(recv) < len(send) {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("Read at %d/%d: %v", len(recv), len(send), err)
		}
		recv = append(recv, buf[:n]...)
	}
	if string(recv) != send {
		t.Errorf("payload mismatch")
	}
}

func TestServer_BadMethod(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)
	srv, _ := NewServer(ServerConfig{Upstream: upstream.addr})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", resp.StatusCode)
	}
}

func TestServer_MissingSessionID(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)
	srv, _ := NewServer(ServerConfig{Upstream: upstream.addr})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	resp, err := http.Post(hs.URL, "application/octet-stream", strings.NewReader(""))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
}

func TestServer_UpstreamDialFails(t *testing.T) {
	srv, _ := NewServer(ServerConfig{
		Upstream:        "127.0.0.1:1",
		ResponseHoldoff: 10 * time.Millisecond,
		Dialer: func(network, address string) (net.Conn, error) {
			return nil, errors.New("synthetic dial failure")
		},
	})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	req, _ := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(""))
	req.Header.Set("X-Session-Id", "abcdef")
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}
}

func TestServer_SessionReap(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)

	srv, _ := NewServer(ServerConfig{
		Upstream:           upstream.addr,
		ResponseHoldoff:    10 * time.Millisecond,
		SessionIdleTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	req, _ := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(""))
	req.Header.Set("X-Session-Id", "reapme")
	resp, _ := hs.Client().Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if got := srv.SessionCount(); got != 1 {
		t.Fatalf("after first POST: SessionCount = %d; want 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for srv.SessionCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := srv.SessionCount(); got != 0 {
		t.Errorf("after idle timeout: SessionCount = %d; want 0", got)
	}
}

// --- helpers ---

// echoUpstream is a TCP listener that loops every byte back to the sender.
// Used as the meek server's upstream so the client → meek → upstream → meek
// → client round-trip can be verified end-to-end.
type echoUpstream struct {
	listener net.Listener
	addr     string
	wg       sync.WaitGroup
	closed   chan struct{}
}

func newEchoUpstream(t *testing.T) *echoUpstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &echoUpstream{
		listener: l,
		addr:     l.Addr().String(),
		closed:   make(chan struct{}),
	}
	u.wg.Add(1)
	go u.accept()
	return u
}

func (u *echoUpstream) accept() {
	defer u.wg.Done()
	for {
		c, err := u.listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}(c)
	}
}

func (u *echoUpstream) Close() {
	select {
	case <-u.closed:
		return
	default:
		close(u.closed)
	}
	_ = u.listener.Close()
	u.wg.Wait()
}
