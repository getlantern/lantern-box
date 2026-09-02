package group

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tw "github.com/getlantern/twiddle"
)

// recordingHarvester captures what the tap offers without touching a pool file.
type recordingHarvester struct {
	mu      sync.Mutex
	offered [][]byte
	done    chan struct{}
	once    sync.Once
}

func newRecording() *recordingHarvester {
	return &recordingHarvester{done: make(chan struct{})}
}

func (r *recordingHarvester) Offer(rec []byte) (bool, error) {
	r.mu.Lock()
	r.offered = append(r.offered, rec)
	r.mu.Unlock()
	r.once.Do(func() { close(r.done) })
	return true, nil
}

func (r *recordingHarvester) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.offered)
}

// sinkFor wires a recording harvester into a sink and closes it with the test.
func sinkFor(t *testing.T, h helloHarvester) *helloSink {
	t.Helper()
	s := newHelloSink(h)
	t.Cleanup(s.close)
	return s
}

// writeThrough runs one write through a tap over a pipe and returns what the
// far side received, so the tap is proven transparent as well as observant.
func writeThrough(t *testing.T, sink *helloSink, payload []byte) []byte {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })

	got := make([]byte, len(payload))
	read := make(chan error, 1)
	go func() {
		_, err := readFull(server, got)
		read <- err
	}()

	tapped := newHelloTap(client, sink)
	if _, err := tapped.Write(payload); err != nil {
		t.Fatalf("write through the tap: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatalf("far side read: %v", err)
	}
	return got
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := c.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// A real browser hello must be harvested, and must still reach the peer byte
// for byte -- the tap is on the live data path.
func TestHelloTapHarvestsAndPassesThrough(t *testing.T) {
	rec := tw.DefaultPool()[0]
	h := newRecording()
	sink := sinkFor(t, h)

	got := writeThrough(t, sink, rec)
	if string(got) != string(rec) {
		t.Error("the tap altered the bytes the peer received")
	}

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("a real ClientHello was never offered to the harvester")
	}
	h.mu.Lock()
	offered := h.offered[0]
	h.mu.Unlock()
	if string(offered) != string(rec) {
		t.Error("the harvester was offered something other than what was written")
	}
}

// Everything that is not a complete ClientHello must be rejected before
// anything is copied or queued: the tap runs on every tunnelled connection.
func TestHelloTapIgnoresNonHellos(t *testing.T) {
	full := tw.DefaultPool()[0]
	for name, payload := range map[string][]byte{
		"http request":      []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"short":             {0x16, 0x03},
		"wrong record type": append([]byte{0x17}, full[1:]...),
		"not a clienthello": func() []byte {
			b := append([]byte(nil), full...)
			b[5] = 0x02 // server_hello
			return b
		}(),
		"truncated record": full[:len(full)-10],
		"ssl2 junk":        {0x80, 0x1c, 0x01, 0x03, 0x01, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRecording()
			sink := sinkFor(t, h)
			got := writeThrough(t, sink, payload)
			if string(got) != string(payload) {
				t.Error("the tap altered a non-hello payload")
			}
			// The sink is async, so give it a chance to be wrong.
			time.Sleep(50 * time.Millisecond)
			if n := h.count(); n != 0 {
				t.Errorf("offered %d non-hello payloads", n)
			}
		})
	}
}

// Only the first write is examined. A hello appearing later in a connection is
// not a browser opening a handshake, and checking every write would put a scan
// on every byte the tunnel carries.
func TestHelloTapOnlyExaminesTheFirstWrite(t *testing.T) {
	rec := tw.DefaultPool()[0]
	h := newRecording()
	sink := sinkFor(t, h)

	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	tapped := newHelloTap(client, sink)
	if _, err := tapped.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := tapped.Write(rec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := h.count(); n != 0 {
		t.Errorf("harvested %d hellos from a non-first write", n)
	}
}

// With no device pool configured the tap must not exist at all: the connection
// comes back unwrapped, so there is no per-write cost for the vast majority of
// clients that have not enabled it.
func TestHelloTapIsAbsentWithoutAPath(t *testing.T) {
	if s := newDevicePoolSink(""); s != nil {
		t.Fatal("an empty device pool path produced a sink")
	}
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	if got := newHelloTap(client, nil); got != client {
		t.Errorf("newHelloTap wrapped the conn despite a nil sink: %T", got)
	}
	// close on a nil sink must be safe, since Close runs unconditionally.
	var nilSink *helloSink
	nilSink.close()
	nilSink.offer([]byte("x"))
}

// The sink must never block the connection path. With the worker wedged, offers
// fill the queue and then drop rather than making Write wait.
func TestHelloSinkDropsRatherThanBlocking(t *testing.T) {
	release := make(chan struct{})
	blocked := &blockingHarvester{release: release}
	sink := newHelloSink(blocked)
	t.Cleanup(sink.close)

	done := make(chan struct{})
	go func() {
		for i := 0; i < helloSinkQueue*20; i++ {
			sink.offer([]byte{0x16})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("offer blocked; the connection path can stall on harvesting")
	}
	close(release)
}

type blockingHarvester struct{ release chan struct{} }

func (b *blockingHarvester) Offer(rec []byte) (bool, error) {
	<-b.release
	return false, nil
}

// End to end against a real harvester: what the tap sees off the wire must end
// up in a pool file that twiddle then loads as the device tier -- and must not
// carry the SNI of the site the write was destined for.
func TestHelloTapFeedsTheDevicePool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.hex")

	h, err := tw.ParseClientHello(tw.DefaultPool()[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetSNI("private.example"); err != nil {
		t.Fatal(err)
	}
	rec := h.Marshal()

	harvester := tw.NewHarvester(path, 0)
	sink := sinkFor(t, harvester)
	writeThrough(t, sink, rec)

	deadline := time.Now().Add(3 * time.Second)
	for harvester.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if harvester.Len() == 0 {
		t.Fatal("nothing reached the device pool")
	}

	pool, err := tw.LoadPool(tw.Sources{Device: path})
	if err != nil {
		t.Fatal(err)
	}
	if pool.Origin != tw.OriginDevice {
		t.Fatalf("pool origin %v, want device", pool.Origin)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsHex(body, "private.example") {
		t.Error("the visited site's name reached the pool file")
	}
}

func containsHex(body []byte, s string) bool {
	const digits = "0123456789abcdef"
	enc := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		enc = append(enc, digits[s[i]>>4], digits[s[i]&0x0f])
	}
	return len(enc) > 0 && indexOf(body, enc) >= 0
}

func indexOf(hay, needle []byte) int {
outer:
	for i := 0; i+len(needle) <= len(hay); i++ {
		for j := range needle {
			if hay[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
