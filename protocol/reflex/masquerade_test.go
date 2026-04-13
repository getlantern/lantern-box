package reflex

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// startEchoServer starts a TCP echo server on 127.0.0.1 and returns its
// host:port and a stop function.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	stop := func() {
		_ = ln.Close()
		<-done
	}
	return ln.Addr().String(), stop
}

// TestForwardToMasquerade_ReplayPrefixAndForward verifies that:
//  1. The prefix bytes (consumed during silence detection) are replayed to
//     upstream before any further client data.
//  2. Upstream's response bytes are forwarded back to the client.
func TestForwardToMasquerade_ReplayPrefixAndForward(t *testing.T) {
	upstream, stop := startEchoServer(t)
	defer stop()

	// net.Pipe() gives us an in-memory, synchronous conn pair. It's enough
	// to exercise io.Copy and close semantics; the upstream half is a real
	// loopback TCP echo server via startEchoServer above.
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	prefix := []byte{'X'}

	forwardErrCh := make(chan error, 1)
	go func() {
		forwardErrCh <- forwardToMasquerade(context.Background(), serverSide, upstream, prefix)
	}()

	// Client writes the rest of "X" + "HELLO" — total stream sent to upstream
	// should be "XHELLO" (echoed back).
	go func() {
		_, _ = clientSide.Write([]byte("HELLO"))
	}()

	buf := make([]byte, 6)
	clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := io.ReadFull(clientSide, buf)
	if err != nil {
		t.Fatalf("read echo: %v (got %q)", err, string(buf[:n]))
	}
	if got := string(buf); got != "XHELLO" {
		t.Fatalf("expected echo 'XHELLO', got %q", got)
	}

	// Closing the client side should drain forwardToMasquerade.
	clientSide.Close()
	select {
	case err := <-forwardErrCh:
		// EOF / closed-pipe is expected; firstRealError suppresses it to nil.
		if err != nil {
			t.Logf("forward returned (acceptable on close): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwardToMasquerade did not return after client close")
	}
}

func TestForwardToMasquerade_DialFailure(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	// Port 1 on localhost should refuse connections quickly.
	err := forwardToMasquerade(context.Background(), serverSide, "127.0.0.1:1", []byte{'X'})
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
}

func TestForwardToMasquerade_EmptyUpstream(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()

	err := forwardToMasquerade(context.Background(), serverSide, "", []byte{'X'})
	if err == nil {
		t.Fatal("expected error for empty upstream, got nil")
	}
}

func TestFirstRealError(t *testing.T) {
	if err := firstRealError(nil, nil); err != nil {
		t.Errorf("expected nil for all-nil, got %v", err)
	}
	if err := firstRealError(io.EOF, io.EOF); err != nil {
		t.Errorf("expected nil for all-EOF, got %v", err)
	}
	real := io.ErrUnexpectedEOF
	if err := firstRealError(io.EOF, real); err != real {
		t.Errorf("expected %v, got %v", real, err)
	}
	if err := firstRealError(real, io.EOF); err != real {
		t.Errorf("expected %v, got %v", real, err)
	}
}
