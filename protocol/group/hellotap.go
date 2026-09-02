package group

import (
	"net"
	"sync"

	tw "github.com/getlantern/twiddle"
)

// helloHarvester is the part of twiddle.Harvester this package needs, kept as
// an interface so the tap is testable without touching a real pool file.
type helloHarvester interface {
	Offer(rec []byte) (bool, error)
}

// helloTap watches the FIRST write on a tunnelled connection for a TLS
// ClientHello and offers it to a twiddle hello harvester.
//
// This is the producer for twiddle's device pool, and it sits here because this
// is the chokepoint: every tunnelled dial passes through a group outbound, so
// the tap sees the device's real browser traffic whatever protocol ends up
// carrying it. Putting it in the twiddle outbound instead would only harvest
// once twiddle was already working, which is backwards -- a stale pool is
// exactly the condition under which twiddle stops working.
//
// Only the first write is examined. Chrome hands its entire ClientHello to the
// socket in a single write (measured: twiddle's
// harvest/testdata/arrival-chrome152.log, 7 of 7 hellos in one write), so
// there is nothing to reassemble, and a first write that is not a complete
// hello means this connection is not a browser TLS handshake and never will be.
// That keeps the tap to one cheap check per connection.
type helloTap struct {
	net.Conn
	sink *helloSink
	once sync.Once
}

func newHelloTap(conn net.Conn, sink *helloSink) net.Conn {
	if sink == nil {
		return conn
	}
	return &helloTap{Conn: conn, sink: sink}
}

func (c *helloTap) Write(b []byte) (int, error) {
	c.once.Do(func() {
		// Copy before handing it off: the caller owns b and may reuse it as
		// soon as Write returns, and the sink reads it on another goroutine.
		if looksLikeClientHello(b) {
			c.sink.offer(append([]byte(nil), b...))
		}
	})
	return c.Conn.Write(b)
}

// looksLikeClientHello screens for a complete handshake record cheaply, before
// anything is copied or queued. twiddle re-validates properly; this only has to
// reject the overwhelming majority of writes, which are not handshakes at all.
func looksLikeClientHello(b []byte) bool {
	if len(b) < 6 || b[0] != 0x16 || b[1] != 0x03 {
		return false
	}
	if b[5] != 0x01 { // handshake type: client_hello
		return false
	}
	return len(b) == 5+int(b[3])<<8+int(b[4])
}

// helloSink runs harvesting off the connection path.
//
// Offer parses, sanitises and -- when it accepts -- writes the pool file, and
// none of that belongs in the latency of a browser's first write. So the tap
// hands records to a single worker through a small buffered channel and DROPS
// when it is full. Dropping costs nothing: harvesting is opportunistic, the
// device produces these constantly, and a full queue means the pool is already
// being fed faster than it can absorb.
type helloSink struct {
	ch   chan []byte
	stop chan struct{}
	once sync.Once
}

// helloSinkQueue is deliberately small. A backlog has no value -- every entry
// in it is a hello from the same browser seconds apart, and the harvester
// dedups interchangeable ones anyway.
const helloSinkQueue = 4

func newHelloSink(h helloHarvester) *helloSink {
	if h == nil {
		return nil
	}
	s := &helloSink{
		ch:   make(chan []byte, helloSinkQueue),
		stop: make(chan struct{}),
	}
	go func() {
		for {
			select {
			case rec := <-s.ch:
				// Errors are pool-file write failures. There is nothing useful
				// to do about them here and they must not affect the
				// connection, which has already been written.
				_, _ = h.Offer(rec)
			case <-s.stop:
				return
			}
		}
	}()
	return s
}

func (s *helloSink) offer(rec []byte) {
	if s == nil {
		return
	}
	select {
	case s.ch <- rec:
	default: // full: drop, see helloSink's doc
	}
}

func (s *helloSink) close() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.stop) })
}

// newDevicePoolSink builds the sink for a device pool path, or nil when no path
// is configured -- in which case newHelloTap returns connections untouched and
// the whole mechanism costs nothing.
func newDevicePoolSink(path string) *helloSink {
	if path == "" {
		return nil
	}
	return newHelloSink(tw.NewHarvester(path, 0))
}
