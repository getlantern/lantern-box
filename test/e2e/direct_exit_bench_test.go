//go:build with_quic && bench

package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
	"github.com/stretchr/testify/require"
)

// BenchmarkPath compares a direct exit (ICE plus Hysteria2 to the volunteer's exit box) with
// today's relayed path (a WebRTC data channel to the volunteer, QUIC on to the egress) over a
// lossy, delayed link between the user and the volunteer. Each path gets one rig; the link's
// loss and delay change under it from case to case.
//
// Loss and delay apply on the volunteer's side only, to the packets between it and the user:
// on the direct exit, to the conn Exit.Serve gets; on the relayed path, to every UDP socket
// the widget's pion opens (its WebRTCOptions.Net). Each datagram either way is dropped with the
// case's loss, and each the volunteer sends waits the case's RTT before it goes. The hop from
// the volunteer to the egress, and the user's side, stay on clean loopback.
//
//	go test -tags 'with_quic bench' -run '^$' -bench BenchmarkPath -benchtime 1x ./test/e2e/ -timeout 30m
func BenchmarkPath(b *testing.B) {
	server := startBenchServer(b)
	for _, path := range []string{"ice", "webrtc"} {
		b.Run(path, func(b *testing.B) {
			link := newLossyLink(time.Now().UnixNano())
			proxy, egressDials := startBenchRig(b, path, server, link)
			for _, loss := range benchLosses() {
				b.Run(fmt.Sprintf("loss=%d", loss), func(b *testing.B) {
					for _, rtt := range benchRTTs() {
						b.Run(fmt.Sprintf("rtt=%d", rtt), func(b *testing.B) {
							link.set(float64(loss)/100, time.Duration(rtt)*time.Millisecond)
							// Let the path's congestion control see the new link before
							// measuring it. Its result is not a measurement.
							_, _, _ = download(proxy, server, cellWarmUpBytes, cellWarmUpCap)
							b.Run("bulk", func(b *testing.B) { benchBulk(b, proxy, server) })
							b.Run("open", func(b *testing.B) { benchOpen(b, proxy, server) })
							b.Run("hol", func(b *testing.B) { benchHOL(b, proxy, server, link, float64(loss)/100) })
							if path == "ice" {
								// A direct exit that failed over to the egress would report
								// the relayed path's numbers as its own.
								require.Zero(b, egressDials.Load(), "the direct exit's traffic went to the egress")
							}
						})
					}
				})
			}
		})
	}
}

// benchLosses and benchRTTs are the matrix: loss in percent, RTT in milliseconds. BENCH_TRIM
// cuts it to 0 and 5% loss, which with 50 and 200 ms is the matrix a short run covers.
func benchLosses() []int {
	if os.Getenv("BENCH_TRIM") != "" {
		return []int{0, 5}
	}
	return []int{0, 2, 5}
}

func benchRTTs() []int { return []int{50, 200} }

const (
	// bulkBytes is what one bulk transfer moves, and the hol case's measured download.
	bulkBytes = 32 << 20
	// transferCap ends a transfer that has not finished, so a slow case can't run the whole
	// matrix past its timeout. Its throughput is then what moved before the cap.
	transferCap = 60 * time.Second
	// cellWarmUpBytes and cellWarmUpCap bound the transfer that runs once a case has set the
	// link, before it measures anything.
	cellWarmUpBytes = 4 << 20
	cellWarmUpCap   = 10 * time.Second
	// opensPerOp is how many streams one open op times.
	opensPerOp = 10
	// holBurst is the extra loss the hol case puts on the user-to-volunteer direction.
	holBurst = 0.05
)

// startBenchRig starts a direct exit rig whose consumer takes the given path, with link
// between the user and the volunteer, checks that the link carries the path, and warms it up
// with a bulk transfer from server. It returns the consumer's proxy address and the egress's
// dial count.
func startBenchRig(b *testing.B, path, server string, link *lossyLink) (string, *atomic.Int64) {
	b.Helper()
	c := directExitCase{enabled: true, router: true, consumerDirectExit: true}
	switch path {
	case "ice":
		c.widget = func(rtcOpt *UBClientcore.WebRTCOptions) {
			serve := rtcOpt.Exit.Serve
			rtcOpt.Exit.Serve = func(ctx context.Context, conn net.PacketConn, password string) {
				lc := newLossyConn(conn, link)
				defer lc.Close()
				serve(ctx, lc, password)
			}
		}
	case "webrtc":
		// The volunteer can't exit, so the consumer takes today's path to the egress.
		c.enabled = false
		base, err := stdnet.NewNet()
		require.NoError(b, err)
		c.widget = func(rtcOpt *UBClientcore.WebRTCOptions) {
			rtcOpt.Net = &lossyNet{Net: base, link: link}
		}
	}
	rig := startDirectExitRig(b, c)

	origin := httptest.NewServer(countingHandler(new(atomic.Int64)))
	b.Cleanup(origin.Close)
	requireProxies(b, rig.proxy, origin.URL, 60*time.Second)
	require.Positive(b, link.packets.Load(), "the link carried none of the %s path's packets", path)
	if path == "ice" {
		require.Positive(b, rig.directSessions.Load(), "the volunteer served no direct exit")
		require.Zero(b, rig.egressDials.Load(), "the direct exit's traffic went to the egress")
	} else {
		require.Zero(b, rig.directSessions.Load(), "the volunteer served a direct exit")
		require.Positive(b, rig.egressDials.Load(), "the relayed path's traffic missed the egress")
	}
	// Warm the path up at the matrix's lowest RTT. Warmed up on clean loopback instead, BBR's
	// minimum RTT would sit near zero, and the first case to add delay would run on a window
	// sized for loopback until that estimate expired, seconds later.
	link.set(0, time.Duration(benchRTTs()[0])*time.Millisecond)
	_, _, err := download(rig.proxy, server, bulkBytes, transferCap)
	require.NoError(b, err, "warm-up transfer")
	return rig.proxy, rig.egressDials
}

// benchBulk times bulkBytes through one stream, from its first byte.
func benchBulk(b *testing.B, proxy, server string) {
	var moved int64
	var took time.Duration
	for i := 0; i < b.N; i++ {
		n, d, err := download(proxy, server, bulkBytes, transferCap)
		require.NoError(b, err)
		moved += n
		took += d
	}
	b.ReportMetric(mbps(moved, took), "MB/s")
}

// benchOpen times opensPerOp new streams each to their first byte.
func benchOpen(b *testing.B, proxy, server string) {
	var took time.Duration
	for i := 0; i < b.N; i++ {
		for j := 0; j < opensPerOp; j++ {
			start := time.Now()
			conn, err := dialBench(proxy, server)
			require.NoError(b, err)
			_, err = conn.Write(downloadRequest(1))
			if err == nil {
				_ = conn.SetReadDeadline(time.Now().Add(transferCap))
				_, err = io.ReadFull(conn, make([]byte, 1))
			}
			took += time.Since(start)
			_ = conn.Close()
			require.NoError(b, err)
		}
	}
	b.ReportMetric(float64(took.Milliseconds())/float64(b.N*opensPerOp), "ms/open")
}

// benchHOL times a bulkBytes download while another stream uploads and holBurst more loss hits
// the upload's direction, user to volunteer, alone. It reports both streams' throughput.
func benchHOL(b *testing.B, proxy, server string, link *lossyLink, loss float64) {
	link.setReadLoss(loss + holBurst)
	defer link.setReadLoss(loss)

	var downMoved, upMoved int64
	var downTook, upTook time.Duration
	for i := 0; i < b.N; i++ {
		up, err := dialBench(proxy, server)
		require.NoError(b, err)
		_, err = up.Write([]byte{'U'})
		require.NoError(b, err)
		var sent atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		upStart := time.Now()
		go func() {
			defer wg.Done()
			chunk := make([]byte, 32<<10)
			for {
				n, err := up.Write(chunk)
				sent.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
		n, d, err := download(proxy, server, bulkBytes, transferCap)
		upTook += time.Since(upStart)
		upMoved += sent.Load()
		_ = up.Close()
		wg.Wait()
		require.NoError(b, err)
		downMoved += n
		downTook += d
	}
	b.ReportMetric(mbps(downMoved, downTook), "MB/s")
	b.ReportMetric(mbps(upMoved, upTook), "up-MB/s")
}

func mbps(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / (1 << 20) / d.Seconds()
}

// download asks server, through proxy, for n bytes on a new stream and reads them, timing
// from the first byte. It stops after limit and returns what moved by then.
func download(proxy, server string, n int64, limit time.Duration) (int64, time.Duration, error) {
	conn, err := dialBench(proxy, server)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	if _, err := conn.Write(downloadRequest(n)); err != nil {
		return 0, 0, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		return 0, 0, err
	}
	start := time.Now()
	got, err := io.CopyN(io.Discard, conn, n-1)
	took := time.Since(start)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		err = nil
	}
	return got + 1, took, err
}

func dialBench(proxy, server string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := socks.NewClient(N.SystemDialer, M.ParseSocksaddr(proxy), socks.Version5, "", "")
	return client.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr(server))
}

func downloadRequest(n int64) []byte {
	return binary.BigEndian.AppendUint64([]byte{'D'}, uint64(n))
}

// startBenchServer listens on loopback for the benchmark's streams: 'D' and a big-endian
// uint64 n asks for n bytes, and 'U' sends bytes it discards. It returns its address.
func startBenchServer(b *testing.B) string {
	b.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(b, err)
	b.Cleanup(func() { _ = l.Close() })
	payload := bytes.Repeat([]byte("unbounded direct exit bench "), 4096)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var mode [1]byte
				if _, err := io.ReadFull(conn, mode[:]); err != nil {
					return
				}
				switch mode[0] {
				case 'U':
					_, _ = io.Copy(io.Discard, conn)
				case 'D':
					var n [8]byte
					if _, err := io.ReadFull(conn, n[:]); err != nil {
						return
					}
					for left := int64(binary.BigEndian.Uint64(n[:])); left > 0; {
						chunk := payload[:min(left, int64(len(payload)))]
						if _, err := conn.Write(chunk); err != nil {
							return
						}
						left -= int64(len(chunk))
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

// lossyLink is what a set of lossyConns do to their datagrams: drop each read with probability
// readLoss and each write with probability writeLoss, and hold each write they keep for delay.
// Its settings can change while its conns run.
type lossyLink struct {
	mu                  sync.Mutex
	rand                *rand.Rand
	readLoss, writeLoss float64
	delay               time.Duration

	// packets counts the datagrams the link's conns saw, and dropped those they dropped.
	packets, dropped atomic.Int64
}

func newLossyLink(seed int64) *lossyLink {
	return &lossyLink{rand: rand.New(rand.NewSource(seed))}
}

// set drops loss of the datagrams both ways and holds writes for delay.
func (l *lossyLink) set(loss float64, delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readLoss, l.writeLoss, l.delay = loss, loss, delay
}

// setReadLoss changes only the loss on reads.
func (l *lossyLink) setReadLoss(loss float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.readLoss = loss
}

// decide reports whether to drop one datagram, read or written, and how long to hold it if not.
func (l *lossyLink) decide(read bool) (drop bool, delay time.Duration) {
	l.packets.Add(1)
	l.mu.Lock()
	loss := l.writeLoss
	if read {
		loss = l.readLoss
	}
	drop = loss > 0 && l.rand.Float64() < loss
	delay = l.delay
	l.mu.Unlock()
	if drop {
		l.dropped.Add(1)
	}
	if read {
		delay = 0
	}
	return drop, delay
}

// lossyConn drops each datagram with its link's loss and delays the rest of those it writes by
// the link's delay, in order, without blocking its writer. Reads aren't delayed: with one side
// of a path wrapped, its writes carry the whole RTT.
type lossyConn struct {
	net.PacketConn
	link  *lossyLink
	queue chan delayedDatagram
	done  chan struct{}
	once  sync.Once
}

type delayedDatagram struct {
	due  time.Time
	p    []byte
	addr net.Addr
}

func newLossyConn(pc net.PacketConn, link *lossyLink) *lossyConn {
	c := &lossyConn{
		PacketConn: pc,
		link:       link,
		// Room for a fast path's whole bandwidth-delay product at 200 ms, so the queue is not
		// what limits throughput.
		queue: make(chan delayedDatagram, 1<<16),
		done:  make(chan struct{}),
	}
	go c.pump()
	return c
}

func (c *lossyConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if drop, _ := c.link.decide(true); !drop {
			return n, addr, nil
		}
	}
}

func (c *lossyConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	drop, delay := c.link.decide(false)
	if drop {
		return len(p), nil
	}
	if delay == 0 {
		return c.PacketConn.WriteTo(p, addr)
	}
	select {
	case c.queue <- delayedDatagram{due: time.Now().Add(delay), p: bytes.Clone(p), addr: addr}:
		return len(p), nil
	case <-c.done:
		return 0, net.ErrClosed
	}
}

func (c *lossyConn) pump() {
	timer := time.NewTimer(0)
	<-timer.C
	for {
		select {
		case <-c.done:
			return
		case d := <-c.queue:
			if wait := time.Until(d.due); wait > 0 {
				timer.Reset(wait)
				select {
				case <-timer.C:
				case <-c.done:
					return
				}
			}
			_, _ = c.PacketConn.WriteTo(d.p, d.addr)
		}
	}
}

func (c *lossyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.PacketConn.Close()
}

// lossyNet is a pion Net whose UDP sockets are lossyConns on one link.
type lossyNet struct {
	transport.Net
	link *lossyLink
}

func (n *lossyNet) ListenPacket(network, address string) (net.PacketConn, error) {
	pc, err := n.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	return newLossyConn(pc, n.link), nil
}

func (n *lossyNet) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	return &lossyUDPConn{UDPConn: c, lc: newLossyConn(c, n.link)}, nil
}

// lossyUDPConn is a pion UDPConn whose ReadFrom and WriteTo, the only methods ICE's
// candidates use, go through a lossyConn.
type lossyUDPConn struct {
	transport.UDPConn
	lc *lossyConn
}

func (c *lossyUDPConn) ReadFrom(p []byte) (int, net.Addr, error)     { return c.lc.ReadFrom(p) }
func (c *lossyUDPConn) WriteTo(p []byte, addr net.Addr) (int, error) { return c.lc.WriteTo(p, addr) }
func (c *lossyUDPConn) Close() error                                 { return c.lc.Close() }

// udpPair is two loopback UDP sockets, a writing to b.
func udpPair(t testing.TB) (a, b net.PacketConn) {
	t.Helper()
	a, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err = net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return a, b
}

func TestLossyConnDropsAboutItsShare(t *testing.T) {
	a, b := udpPair(t)
	link := newLossyLink(1)
	link.set(0.05, 0)
	lc := newLossyConn(a, link)
	t.Cleanup(func() { _ = lc.Close() })

	const writes = 10000
	for i := 0; i < writes; i++ {
		n, err := lc.WriteTo([]byte("datagram"), b.LocalAddr())
		require.NoError(t, err)
		require.Equal(t, len("datagram"), n, "a dropped write still reports success")
	}
	dropped := link.dropped.Load()
	require.GreaterOrEqual(t, dropped, int64(400))
	require.LessOrEqual(t, dropped, int64(600))
	require.Equal(t, int64(writes), link.packets.Load())
}

func TestLossyConnDropsReads(t *testing.T) {
	a, b := udpPair(t)
	link := newLossyLink(1)
	link.set(0, 0)
	link.setReadLoss(1)
	lc := newLossyConn(b, link)
	t.Cleanup(func() { _ = lc.Close() })

	_, err := a.WriteTo([]byte("x"), b.LocalAddr())
	require.NoError(t, err)
	require.NoError(t, lc.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, _, err = lc.ReadFrom(make([]byte, 16))
	require.Error(t, err, "a read the link drops must never be delivered")
	require.Equal(t, int64(1), link.dropped.Load())
}

func TestLossyConnDelaysWrites(t *testing.T) {
	a, b := udpPair(t)
	link := newLossyLink(1)
	link.set(0, 80*time.Millisecond)
	lc := newLossyConn(a, link)
	t.Cleanup(func() { _ = lc.Close() })

	start := time.Now()
	for _, p := range []string{"one", "two", "three"} {
		_, err := lc.WriteTo([]byte(p), b.LocalAddr())
		require.NoError(t, err)
	}
	require.Less(t, time.Since(start), 40*time.Millisecond, "a delayed write blocked its writer")

	buf := make([]byte, 16)
	require.NoError(t, b.SetReadDeadline(time.Now().Add(2*time.Second)))
	for _, want := range []string{"one", "two", "three"} {
		n, _, err := b.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, want, string(buf[:n]), "delayed writes keep their order")
	}
	require.GreaterOrEqual(t, time.Since(start), 80*time.Millisecond)
}
