package unbounded

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
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

// spentLimitedConn is a limited socket whose shared write bucket is empty, at
// 64 KiB/s, with a sink to write to.
func spentLimitedConn(t *testing.T) (transport.UDPConn, net.Addr) {
	t.Helper()
	ln, err := newLimitedNet(64 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	c, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for sent := 0; sent < minBurst; sent += 8 * 1024 {
		if _, err := c.WriteTo(make([]byte, 8*1024), sink.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	return c, sink.LocalAddr()
}

// Extending the deadline while a write waits on the limiter lets the write
// finish, as it would for the socket's own I/O.
func TestLimitedNetWaitFollowsAnExtendedDeadline(t *testing.T) {
	c, sink := spentLimitedConn(t)
	if err := c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteTo(make([]byte, 8*1024), sink) // ~125ms of tokens
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write failed after its deadline was extended: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write never finished")
	}
}

// Setting the deadline to now while a write waits aborts it at once, which is
// how pion's UDP mux unblocks a stuck write.
func TestLimitedNetWaitAbortsOnADeadlineSetToNow(t *testing.T) {
	c, sink := spentLimitedConn(t)
	done := make(chan error, 1)
	go func() {
		_, err := c.WriteTo(make([]byte, 8*1024), sink)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if err := c.SetWriteDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write returned %v, want os.ErrDeadlineExceeded", err)
		}
		if waited := time.Since(start); waited > 50*time.Millisecond {
			t.Fatalf("write took %v to see its deadline", waited)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write kept waiting past a deadline set to now")
	}
}

// Interface lookups take a fresh snapshot each time, so a long-lived Net sees
// network changes.
func TestLimitedNetInterfacesAreFresh(t *testing.T) {
	ln, err := newLimitedNet(64 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	var made atomic.Int32
	prev := newStdNet
	newStdNet = func() (*stdnet.Net, error) {
		made.Add(1)
		return prev()
	}
	t.Cleanup(func() { newStdNet = prev })
	for range 2 {
		if _, err := ln.Interfaces(); err != nil {
			t.Fatal(err)
		}
	}
	if made.Load() != 2 {
		t.Fatalf("made %d snapshots for 2 lookups, want 2", made.Load())
	}
}

// slowDeadlineConn records the last write deadline set on it, pausing first so
// concurrent setters interleave.
type slowDeadlineConn struct {
	transport.UDPConn
	mu   sync.Mutex
	last time.Time
}

func (c *slowDeadlineConn) SetWriteDeadline(t time.Time) error {
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	c.last = t
	c.mu.Unlock()
	return nil
}

// Concurrent deadline setters leave the limiter's deadline and the socket's
// the same, whichever finishes last.
func TestLimitedNetDeadlineUpdatesAreAtomic(t *testing.T) {
	for range 20 {
		inner := &slowDeadlineConn{}
		c := &limitedUDPConn{UDPConn: inner, limits: newConnLimits(newLimiter(1<<20), newLimiter(1<<20))}
		var wg sync.WaitGroup
		base := time.Now()
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.SetWriteDeadline(base.Add(time.Duration(i) * time.Second))
			}()
		}
		wg.Wait()
		inner.mu.Lock()
		socket := inner.last
		inner.mu.Unlock()
		if limiter := c.limits.deadlineOf(true).at; !limiter.Equal(socket) {
			t.Fatalf("limiter's write deadline %v, socket's %v", limiter, socket)
		}
	}
}

// A deadline extended after a wait read it, but before the wait acted on it,
// is honoured: the wait doesn't time out on the old value.
func TestLimitedNetWaitRechecksAStaleDeadline(t *testing.T) {
	c, sink := spentLimitedConn(t)
	if err := c.SetWriteDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Once the wait has read the 30ms deadline, let it pass, then extend it.
	var once sync.Once
	afterDeadlineRead = func() {
		once.Do(func() {
			time.Sleep(50 * time.Millisecond)
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		})
	}
	t.Cleanup(func() { afterDeadlineRead = nil })
	if _, err := c.WriteTo(make([]byte, 8*1024), sink); err != nil {
		t.Fatalf("write timed out on a deadline that had been extended: %v", err)
	}
}

// Receiving through two limited sockets on one Net takes about the time the
// shared receive limit implies, once the burst is spent.
func TestLimitedNetShapesReadsAcrossSockets(t *testing.T) {
	const limit = byteRate(256 * 1024)
	ln, err := newLimitedNet(limit)
	if err != nil {
		t.Fatal(err)
	}
	var receivers []transport.UDPConn
	for range 2 {
		c, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		receivers = append(receivers, c)
	}
	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	const packet = 8 * 1024
	const perSocket = 12 // 192 KiB in all, three times the 64 KiB burst
	start := time.Now()
	errs := make(chan error, len(receivers))
	for _, r := range receivers {
		go func() {
			// One datagram in flight per socket, so nothing depends on the
			// host's receive buffer size.
			buf := make([]byte, packet)
			if err := r.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				errs <- err
				return
			}
			for i := 0; i < perSocket; i++ {
				if _, err := sender.WriteTo(make([]byte, packet), r.LocalAddr()); err != nil {
					errs <- err
					return
				}
				if _, _, err := r.ReadFrom(buf); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range receivers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	total := 2 * perSocket * packet
	want := time.Duration(float64(total-newLimiter(limit).Burst()) / float64(limit) * float64(time.Second))
	if elapsed < want*7/10 {
		t.Fatalf("received %d bytes in %v; the shared limit should have taken at least %v", total, elapsed, want)
	}
	if elapsed > want*3 {
		t.Fatalf("received %d bytes in %v; far slower than the limit's %v", total, elapsed, want)
	}
}

// A read whose receive wait is interrupted, by its socket closing or its
// deadline moving to now, still returns the datagram it already has.
func TestLimitedNetInterruptedReadKeepsTheDatagram(t *testing.T) {
	for name, interrupt := range map[string]func(transport.UDPConn){
		"close":    func(c transport.UDPConn) { c.Close() },
		"deadline": func(c transport.UDPConn) { _ = c.SetReadDeadline(time.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			// At 1 KiB/s an 8 KiB datagram past the burst takes about 8s of
			// tokens, so the read is certain to be waiting when interrupted.
			ln, err := newLimitedNet(1024)
			if err != nil {
				t.Fatal(err)
			}
			c, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			buf := make([]byte, 8*1024)
			// Spend the receive burst, then send one more datagram.
			for got := 0; got < minBurst; got += len(buf) {
				if _, err := sender.WriteTo(buf, c.LocalAddr()); err != nil {
					t.Fatal(err)
				}
				if _, _, err := c.ReadFrom(buf); err != nil {
					t.Fatal(err)
				}
			}
			// Hooked only now, so it signals the final read's wait.
			waiting := make(chan struct{})
			var once sync.Once
			afterDeadlineRead = func() { once.Do(func() { close(waiting) }) }
			t.Cleanup(func() { afterDeadlineRead = nil })
			last := make([]byte, len(buf))
			copy(last, "last")
			if _, err := sender.WriteTo(last, c.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			go func() {
				n, _, err := c.ReadFrom(buf)
				done <- result{n, err}
			}()
			select {
			case <-waiting:
			case r := <-done:
				t.Fatalf("the read returned (%d, %v) without waiting on the limiter", r.n, r.err)
			case <-time.After(5 * time.Second):
				t.Fatal("the read never reached the limiter")
			}
			interrupt(c)
			select {
			case r := <-done:
				if r.err != nil || r.n != len(last) || string(buf[:4]) != "last" {
					t.Fatalf("read returned %d bytes starting %q, %v; want the datagram", r.n, buf[:4], r.err)
				}
			case <-time.After(time.Second):
				t.Fatal("the read kept waiting after it was interrupted")
			}
		})
	}
}

// A socket on a multicast address, which only pion's mDNS opens, is the host's
// own: x/net reads it only as a *net.UDPConn. Every other socket is limited.
func TestLimitedNetLeavesMulticastSocketsUnwrapped(t *testing.T) {
	ln, err := newLimitedNet(64 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	mdns, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)})
	if err != nil {
		t.Skipf("can't listen on a multicast address here: %v", err)
	}
	defer mdns.Close()
	if _, ok := mdns.(*net.UDPConn); !ok {
		t.Fatalf("multicast socket is %T, want *net.UDPConn", mdns)
	}
	unicast, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer unicast.Close()
	if _, ok := unicast.(*limitedUDPConn); !ok {
		t.Fatalf("unicast socket is %T, want it limited", unicast)
	}
}

// A write on a closed socket, or past its deadline, fails without spending
// tokens other sockets share.
func TestLimitedNetFailedWritesSpendNoTokens(t *testing.T) {
	ln, err := newLimitedNet(64 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	tx := ln.(*limitedNet).tx
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	for name, spoil := range map[string]func(transport.UDPConn){
		"closed":  func(c transport.UDPConn) { c.Close() },
		"expired": func(c transport.UDPConn) { _ = c.SetWriteDeadline(time.Now().Add(-time.Second)) },
	} {
		t.Run(name, func(t *testing.T) {
			c, err := ln.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			spoil(c)
			before := tx.Tokens()
			if _, err := c.WriteTo(make([]byte, 8*1024), sink.LocalAddr()); err == nil {
				t.Fatal("the write succeeded")
			}
			if after := tx.Tokens(); after < before-1 {
				t.Fatalf("a failed write spent %.0f tokens", before-after)
			}
		})
	}
}

// A deadline extended after the check before reserving read it, but before
// the check acted on it, is honoured too.
func TestLimitedNetEarlyCheckRechecksAStaleDeadline(t *testing.T) {
	c, sink := spentLimitedConn(t)
	if err := c.SetWriteDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	afterEarlyDeadlineRead = func() {
		once.Do(func() {
			time.Sleep(50 * time.Millisecond)
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		})
	}
	t.Cleanup(func() { afterEarlyDeadlineRead = nil })
	if _, err := c.WriteTo(make([]byte, 8*1024), sink); err != nil {
		t.Fatalf("write timed out on a deadline that had been extended: %v", err)
	}
}

// failingConn is a socket whose writes fail.
type failingConn struct{ transport.UDPConn }

func (failingConn) WriteTo([]byte, net.Addr) (int, error) { return 0, errors.New("write failed") }

// Tokens granted for a write that then fails go back, so unsent bytes cost
// the other sockets nothing.
func TestLimitedNetFailedWriteIsRefunded(t *testing.T) {
	tx := newTokenBucket(1024, 64*1024)
	c := &limitedUDPConn{UDPConn: failingConn{}, limits: newConnLimits(tx, newTokenBucket(1024, 64*1024))}
	before := tx.Tokens()
	if _, err := c.WriteTo(make([]byte, 8*1024), &net.UDPAddr{}); err == nil {
		t.Fatal("the write succeeded")
	}
	if after := tx.Tokens(); after < before-1 {
		t.Fatalf("a failed write kept %.0f tokens", before-after)
	}
}
