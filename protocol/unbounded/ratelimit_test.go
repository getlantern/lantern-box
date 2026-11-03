package unbounded

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/transport/v5"
)

// Sending through a limited socket must take about the time the limit
// implies: 512 KiB at 1 MiB/s, less the burst admitted up front.
func TestLimitedNetShapesWrites(t *testing.T) {
	const limit = byteRate(1 << 20)
	ln, err := newLimitedNet(limit)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	const packet = 1024
	const packets = 512
	buf := make([]byte, packet)
	start := time.Now()
	for i := 0; i < packets; i++ {
		if _, err := sender.WriteTo(buf, sink.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)

	// The burst is admitted at once; the rest is paced at the limit.
	burst := newLimiter(limit).Burst()
	want := time.Duration(float64(packets*packet-burst) / float64(limit) * float64(time.Second))
	if elapsed < want*8/10 {
		t.Fatalf("sent %d bytes in %v; limit should have taken at least %v", packets*packet, elapsed, want)
	}
	if elapsed > want*3 {
		t.Fatalf("sent %d bytes in %v; far slower than the limit's %v", packets*packet, elapsed, want)
	}
}

func TestLimitedNetOversizedWriteIsNotBlocked(t *testing.T) {
	ln, err := newLimitedNet(1000)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	done := make(chan error, 1)
	go func() {
		_, err := sender.WriteTo(make([]byte, minBurst+1), sink.LocalAddr())
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a datagram larger than the burst must not block forever")
	}
}

// A write waiting on a spent bucket must return as soon as the socket is
// closed or its write deadline passes, not when the tokens arrive.
func TestLimitedNetWaitsAreInterruptible(t *testing.T) {
	ln, err := newLimitedNet(1000) // 1 KB/s: the 64 KiB burst takes a minute to refill
	if err != nil {
		t.Fatal(err)
	}
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	// Spend the whole burst in datagrams a UDP socket accepts: 8 KiB fits
	// under macOS's 9216-byte default as well as Linux's limit.
	const chunk = 8 * 1024
	spend := func(c transport.UDPConn) {
		t.Helper()
		for sent := 0; sent < minBurst; sent += chunk {
			if _, err := c.WriteTo(make([]byte, chunk), sink.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
	}

	closed, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	spend(closed)
	done := make(chan error, 1)
	go func() {
		_, err := closed.WriteTo(make([]byte, 1024), sink.LocalAddr())
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	closed.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write after close returned %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write kept waiting on the limiter after the socket was closed")
	}

	timed, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer timed.Close()
	if err := timed.SetWriteDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = timed.WriteTo(make([]byte, 1024), sink.LocalAddr())
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write past the deadline returned %v, want os.ErrDeadlineExceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("deadline did not interrupt the limiter wait")
	}
}
