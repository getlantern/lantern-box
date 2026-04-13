package reflex

import (
	"net"
	"testing"
	"time"
)

func TestWaitForSilence_Timeout(t *testing.T) {
	// Client connects but sends no data — expect (nil, nil) after timeout.
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	start := time.Now()
	data, err := waitForSilence(server, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error on silence, got %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil data on silence, got %q", data)
	}
	if elapsed < 90*time.Millisecond {
		t.Fatalf("returned too early (%v); should have waited for timeout", elapsed)
	}
}

func TestWaitForSilence_ClientSpeaks(t *testing.T) {
	// Client sends a byte — expect (data, nil).
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = client.Write([]byte{'X'})
	}()

	data, err := waitForSilence(server, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error when client speaks, got %v", err)
	}
	if len(data) != 1 || data[0] != 'X' {
		t.Fatalf("expected [X], got %q", data)
	}
}

func TestWaitForSilence_ClearsDeadline(t *testing.T) {
	// After waitForSilence returns, the read deadline should be cleared so
	// subsequent reads (e.g., TLS handshake) can proceed.
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	_, err := waitForSilence(server, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Writing from client, reading from server should now work without
	// immediate deadline failure.
	go func() {
		_, _ = client.Write([]byte("hello"))
	}()

	buf := make([]byte, 5)
	server.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, err := server.Read(buf)
	if err != nil {
		t.Fatalf("expected read to succeed after clearing deadline, got %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}
}

func TestJitteredTimeout_Bounds(t *testing.T) {
	base := 5 * time.Second
	jitter := 2 * time.Second

	for i := 0; i < 100; i++ {
		got := jitteredTimeout(base, jitter)
		if got < base-jitter || got > base+jitter {
			t.Fatalf("got %v, expected within [%v, %v]", got, base-jitter, base+jitter)
		}
	}
}

func TestJitteredTimeout_NoJitter(t *testing.T) {
	got := jitteredTimeout(5*time.Second, 0)
	if got != 5*time.Second {
		t.Fatalf("expected base when jitter is 0, got %v", got)
	}
}
