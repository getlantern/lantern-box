package meek

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faultTransport simulates a LOST RESPONSE: it lets the request reach the meek
// server (so the server processes that seq — drains downstream / writes upstream)
// but then discards the response and reports an error to the client. That forces
// the client to retry the same seq, which is exactly the case a naive retry gets
// wrong: the server already advanced, so a correct implementation must replay the
// buffered response (no gap downstream, no duplicate upstream).
type faultTransport struct {
	inner     http.RoundTripper
	n         int64
	dropEvery int64 // drop the response on every Nth request (0 = never)
	dropped   int64
}

func (f *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := atomic.AddInt64(&f.n, 1)
	resp, err := f.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if f.dropEvery > 0 && i%f.dropEvery == 0 {
		// The server has already handled this seq; drop its response so the
		// client must retry and rely on the server replaying it.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		atomic.AddInt64(&f.dropped, 1)
		return nil, fmt.Errorf("injected response loss for request %d", i)
	}
	return resp, nil
}

func newMeekTestStack(t *testing.T, upstream string, dropEvery int64) (*Conn, *faultTransport) {
	t.Helper()
	srv, err := NewServer(ServerConfig{Upstream: upstream})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	ft := &faultTransport{inner: http.DefaultTransport, dropEvery: dropEvery}
	hc := &http.Client{Transport: ft, Timeout: 10 * time.Second}
	conn, err := Dial(context.Background(), Config{
		URL: hs.URL, InnerHost: "test", HTTPClient: hc,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, ft
}

// TestMeekRetryDownloadIntegrity: upstream streams a 2 MiB random payload; every
// 4th poll response is dropped. The client must reassemble the payload byte-for-
// byte — a gap (lost-after-drain) or dup would fail the compare.
func TestMeekRetryDownloadIntegrity(t *testing.T) {
	const size = 2 << 20
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				go io.Copy(io.Discard, c) // drain the client's trigger byte(s)
				_, _ = c.Write(payload)
			}(c)
		}
	}()

	conn, ft := newMeekTestStack(t, ln.Addr().String(), 4)
	if _, err := conn.Write([]byte("go")); err != nil { // trigger upstream
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	got := make([]byte, 0, size)
	buf := make([]byte, 96*1024)
	for len(got) < size {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("read err at %d/%d bytes: %v", len(got), size, err)
		}
	}
	if !bytes.Equal(got[:size], payload) {
		t.Fatalf("download corrupted under response loss (got %d bytes)", len(got))
	}
	if d := atomic.LoadInt64(&ft.dropped); d == 0 {
		t.Fatal("test ineffective: no responses were dropped")
	} else {
		t.Logf("download intact across %d dropped+retried responses", d)
	}
}

// TestMeekRetryUploadNoDuplication: the client writes a 2 MiB random payload; the
// upstream concatenates everything it receives. With response loss forcing
// retries, a non-idempotent server would re-write retried chunks → upstream sees
// >2 MiB / corrupted bytes. Correct dedupe yields exactly the payload.
func TestMeekRetryUploadNoDuplication(t *testing.T) {
	const size = 2 << 20
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	recv := make([]byte, size)
	var readErr error
	var extra bool
	done := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Read exactly `size` bytes (no client Close needed — closing would
		// cancel the poll loop and drop unsent data), then check that no further
		// bytes arrive: a duplicated retry chunk would show up as trailing data.
		if _, readErr = io.ReadFull(c, recv); readErr != nil {
			close(done)
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
		if n, _ := c.Read(make([]byte, 1)); n > 0 {
			extra = true
		}
		close(done)
	}()

	conn, ft := newMeekTestStack(t, ln.Addr().String(), 4)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("upstream did not receive full payload in time")
	}
	if readErr != nil {
		t.Fatalf("upstream read: %v (gap under retry?)", readErr)
	}
	if extra {
		t.Fatal("upstream received MORE than payload — a retried chunk was duplicated")
	}
	if !bytes.Equal(recv, payload) {
		t.Fatal("upstream bytes don't match payload (corruption under retry)")
	}
	if d := atomic.LoadInt64(&ft.dropped); d == 0 {
		t.Fatal("test ineffective: no responses were dropped")
	}
	t.Logf("upload intact (exactly %d bytes) across %d dropped+retried responses", size, atomic.LoadInt64(&ft.dropped))
}

// brokenUpstream serves some bytes and then fails the read, which is what an
// RST or an I/O error on the origin looks like to the session's read pump.
type brokenUpstream struct {
	net.Conn
	remaining int
	failure   error
}

func (c *brokenUpstream) Read(b []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, c.failure
	}
	n := len(b)
	if n > c.remaining {
		n = c.remaining
	}
	for i := range b[:n] {
		b[i] = 0x5a
	}
	c.remaining -= n
	return n, nil
}

func (c *brokenUpstream) Write(b []byte) (int, error) { return len(b), nil }
func (c *brokenUpstream) Close() error                { return nil }

// A truncated download must not reach the caller as a clean end of stream.
//
// readPump ends on any upstream read error, and closing upstreamDone is what
// the session reads as "closed with nothing left" -- so before this was split
// out, a failed read became a 410, which the client maps to io.EOF. The caller
// could not tell a finished transfer from a broken one, and the tail of the
// byte stream vanished silently. Everything else in this package is careful
// about exactly that (see the over-cap check in client.go), so it should be
// careful here too.
func TestBrokenUpstreamIsNotReportedAsCleanEOF(t *testing.T) {
	const served = 4096
	failure := errors.New("upstream exploded")

	sess := &session{
		id:           "t",
		upstream:     &brokenUpstream{remaining: served, failure: failure},
		readWakeCh:   make(chan struct{}, 1),
		upstreamDone: make(chan struct{}),
	}
	sess.drainCond = sync.NewCond(&sess.mu)
	go sess.readPump(1 << 20)

	// Drain what the upstream did deliver.
	var got int
	for got < served {
		resp, err := sess.serveRequest(true, uint64(got), nil, 1024, 20*time.Millisecond)
		if err != nil {
			t.Fatalf("failed while bytes were still buffered, at %d/%d: %v", got, served, err)
		}
		if len(resp) == 0 {
			continue
		}
		got += len(resp)
	}

	// The upstream is now finished, and it finished badly.
	var err error
	for i := 0; i < 50 && err == nil; i++ {
		_, err = sess.serveRequest(true, uint64(served+i+1), nil, 1024, 20*time.Millisecond)
	}
	if err == nil {
		t.Fatal("a broken upstream never ended the session")
	}
	if errors.Is(err, errUpstreamClosed) {
		t.Fatal("a broken upstream reported a clean end of stream; the client would surface io.EOF and the caller would read a short, apparently complete stream")
	}
	if !errors.Is(err, errUpstreamBroken) || !errors.Is(err, failure) {
		t.Fatalf("got %v, want it to wrap both errUpstreamBroken and the read failure", err)
	}
}
