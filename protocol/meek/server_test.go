package meek

import (
	"bytes"
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

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/varbin"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/sagernet/sing/protocol/socks/socks5"
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

func TestServer_AuthTokenRequired(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)
	srv, _ := NewServer(ServerConfig{Upstream: upstream.addr, AuthToken: "s3cret"})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	post := func(authHeader string) int {
		req, _ := http.NewRequest(http.MethodPost, hs.URL, strings.NewReader(""))
		req.Header.Set("X-Session-Id", "abcdef")
		if authHeader != "" {
			req.Header.Set("X-Meek-Auth", authHeader)
		}
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(""); got != http.StatusForbidden {
		t.Errorf("missing token: status = %d; want 403", got)
	}
	if got := post("wrong"); got != http.StatusForbidden {
		t.Errorf("wrong token: status = %d; want 403", got)
	}
	if got := post("s3cret"); got == http.StatusForbidden {
		t.Errorf("correct token: status = 403; want the request to proceed")
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

// A POST body larger than MaxBodyBytes must be rejected with 413 rather
// than silently truncated and forwarded upstream, which would corrupt
// the tunneled stream.
func TestServer_RejectsOversizedBody(t *testing.T) {
	upstream := newEchoUpstream(t)
	t.Cleanup(upstream.Close)
	srv, _ := NewServer(ServerConfig{Upstream: upstream.addr, MaxBodyBytes: 512})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	req, _ := http.NewRequest(http.MethodPost, hs.URL, bytes.NewReader(make([]byte, 1024)))
	req.Header.Set("X-Session-Id", "oversized")
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d; want 413", resp.StatusCode)
	}
}

// When MaxBodyBytes is small enough that the read-pump cap (MaxBodyBytes*4)
// is below a single upstream read, an empty pending buffer must still
// accept the chunk; otherwise the pump waits forever and downstream bytes
// never flow. Regression for the cap-vs-read-size deadlock.
func TestServer_SmallMaxBodyBytesDelivers(t *testing.T) {
	const blob = 64 * 1024
	upstream := newBurstUpstream(t, blob)
	t.Cleanup(upstream.Close)

	srv, _ := NewServer(ServerConfig{
		Upstream:        upstream.addr,
		ResponseHoldoff: 10 * time.Millisecond,
		MaxBodyBytes:    1024, // cap = 4096, far below a 64 KiB read
	})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	c, err := Dial(context.Background(), Config{
		URL:          hs.URL,
		HTTPClient:   hs.Client(),
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Send a byte so the session (and its upstream conn) is created; the
	// burst upstream then floods its blob back.
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	c.SetReadDeadline(time.Now().Add(8 * time.Second))
	buf := make([]byte, 16*1024)
	var got int
	for got < blob {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("Read at %d/%d: %v", got, blob, err)
		}
		got += n
	}
}

// The bundled meek outbound opens each session with a SOCKS5 CONNECT to
// the destination over the tunnel. This exercises that full chain —
// client -> meek server -> SOCKS5 upstream -> destination — using the
// same socks.ClientHandshake5 the outbound runs.
func TestServer_SOCKS5ConnectOverTunnel(t *testing.T) {
	dest := newEchoUpstream(t)
	t.Cleanup(dest.Close)

	proxy := newSOCKS5Proxy(t)
	t.Cleanup(proxy.Close)

	srv, _ := NewServer(ServerConfig{
		Upstream:        proxy.addr,
		ResponseHoldoff: 10 * time.Millisecond,
	})
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	c, err := Dial(context.Background(), Config{
		URL:          hs.URL,
		HTTPClient:   hs.Client(),
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := socks.ClientHandshake5(c, socks5.CommandConnect, M.ParseSocksaddr(dest.addr), "", ""); err != nil {
		t.Fatalf("SOCKS5 CONNECT over tunnel: %v", err)
	}

	if _, err := c.Write([]byte("ping through socks")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, 64)
	n, err := c.Read(got)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s := string(got[:n]); s != "ping through socks" {
		t.Errorf("got %q; want %q", s, "ping through socks")
	}
}

// --- helpers ---

// burstUpstream writes a fixed-size blob to every accepted connection
// immediately, then holds the connection open. Used to force a single
// large upstream read on the meek server's read pump.
type burstUpstream struct {
	listener net.Listener
	addr     string
	blob     []byte
	wg       sync.WaitGroup
}

func newBurstUpstream(t *testing.T, n int) *burstUpstream {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &burstUpstream{listener: l, addr: l.Addr().String(), blob: bytes.Repeat([]byte("Z"), n)}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				_, _ = conn.Write(u.blob)
				_, _ = io.Copy(io.Discard, conn)
				conn.Close()
			}(conn)
		}
	}()
	return u
}

func (u *burstUpstream) Close() { _ = u.listener.Close(); u.wg.Wait() }

// socks5Proxy is a minimal no-auth SOCKS5 CONNECT proxy used as a meek
// upstream in tests: it completes the handshake, dials the requested
// destination, and pipes bytes both ways.
type socks5Proxy struct {
	listener net.Listener
	addr     string
	wg       sync.WaitGroup
}

func newSOCKS5Proxy(t *testing.T) *socks5Proxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &socks5Proxy{listener: l, addr: l.Addr().String()}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *socks5Proxy) serve(conn net.Conn) {
	defer conn.Close()
	reader := varbin.StubReader(conn)
	if _, err := socks5.ReadAuthRequest(reader); err != nil {
		return
	}
	if err := socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: socks5.AuthTypeNotRequired}); err != nil {
		return
	}
	req, err := socks5.ReadRequest(reader)
	if err != nil {
		return
	}
	upstream, err := net.Dial("tcp", req.Destination.String())
	if err != nil {
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeFailure})
		return
	}
	defer upstream.Close()
	if err := socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeSuccess}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func (p *socks5Proxy) Close() { _ = p.listener.Close(); p.wg.Wait() }

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
