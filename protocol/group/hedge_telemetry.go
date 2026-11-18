package group

import (
	"context"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	semconv "github.com/getlantern/semconv"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Hedge attribute keys. They are Lantern-specific, so they belong in
// semconv's custom.go; they live here until they are added there.
const (
	// hedgePathKey is where a connection's bytes went: see the hedgePath
	// constants.
	hedgePathKey attribute.Key = "hedge.path"
	// hedgeOutcomeKey is how a connection that tried First ended, or that
	// Then failed to dial on any path.
	hedgeOutcomeKey attribute.Key = "hedge.outcome"
	// hedgeSiteAttrKey is the connection's registered domain, only when it is one
	// of the hedge's telemetry_sites, else "other". Never set otherwise.
	hedgeSiteAttrKey attribute.Key = "hedge.site"
	// hedgeFirstKey and hedgeThenKey are the hedge's outbound tags, with
	// helper tags redacted.
	hedgeFirstKey attribute.Key = "hedge.first"
	hedgeThenKey  attribute.Key = "hedge.then"
)

// Paths a hedged connection can take. Bandit probes take none: they record
// nothing.
const (
	hedgePathFirst = "first"
	// hedgePathThen is Then because First can't carry the network.
	hedgePathThen = "then"
	// hedgePathThenKnownBad is Then because the cache says First fails for
	// the site or its address block.
	hedgePathThenKnownBad = "then_known_bad"
	// hedgePathThenAfterMove is Then after trying First.
	hedgePathThenAfterMove = "then_after_move"
	// hedgePathThenExplore is Then for a site First may work for, chosen at
	// random (explore_rate) to compare the two paths on the same sites.
	hedgePathThenExplore = "then_explore"
)

// Outcomes of connections that tried First, and then_dial_error, which any
// path can have.
const (
	hedgeOutcomeCommitted          = "committed"
	hedgeOutcomeMovedNoReply       = "moved_no_reply"
	hedgeOutcomeMovedReset         = "moved_reset"
	hedgeOutcomeMovedDialError     = "moved_dial_error"
	hedgeOutcomeOverflowFailed     = "overflow_failed"
	hedgeOutcomeStalledAfterCommit = "stalled_after_commit"
	hedgeOutcomeResetAfterCommit   = "reset_after_commit"
	hedgeOutcomeResetNotReplayable = "reset_not_replayable"
	// hedgeOutcomeResetUnmoved is a replayable connection First reset that
	// didn't move: the client closed before the move started, or a reply
	// already on its way committed it. A reset before the client wrote
	// anything counts here too.
	hedgeOutcomeResetUnmoved = "reset_unmoved"
	// hedgeOutcomeResetUndecided is a connection First reset while its
	// opening (one to three bytes) was too short to tell whether it could
	// be replayed.
	hedgeOutcomeResetUndecided = "reset_undecided"
	// hedgeOutcomeMoveFailed is a connection whose move to Then failed: Then
	// couldn't be dialed or didn't take the replayed opening.
	hedgeOutcomeMoveFailed = "move_failed"
	// hedgeOutcomeThenDialError is a connection Then failed to dial, on any
	// path, so Then's failures are counted as First's are.
	hedgeOutcomeThenDialError = "then_dial_error"
	// hedgeOutcomeAbandoned is a connection the client closed before its
	// outcome was decided: before First replied or failed, or while a reply
	// was still on its way, so it can carry bytes and a first-byte time.
	hedgeOutcomeAbandoned = "abandoned"
)

const hedgeSiteOther = "other"

// hedgeTelemetry records one data point set per hedged connection when it
// closes. The attributes are low-cardinality: path, outcome, transport, the
// two outbound tags and, for sites the config names, the site.
type hedgeTelemetry struct {
	connections metric.Int64Counter
	io          metric.Int64Counter
	firstByte   metric.Float64Histogram
}

// hedgeFirstByteBuckets are seconds, from a nearby CDN to a slow fallback.
var hedgeFirstByteBuckets = []float64{0.025, 0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 1.5, 2.5, 5, 10}

// hedgeInstruments holds the instruments of the meter provider they were
// created from. The provider is replaced when telemetry's config changes or
// it is turned off or on, so instruments are looked up per record rather
// than kept from when the hedge was built.
var hedgeInstruments struct {
	mu       sync.Mutex
	provider metric.MeterProvider
	tel      *hedgeTelemetry
}

// currentHedgeTelemetry returns the instruments of the current global meter
// provider, or nil when they can't be created.
func currentHedgeTelemetry() *hedgeTelemetry {
	mp := otel.GetMeterProvider()
	hedgeInstruments.mu.Lock()
	defer hedgeInstruments.mu.Unlock()
	// A provider whose type can't be compared would make != panic; such a
	// provider gets fresh instruments on every record instead.
	comparable := reflect.TypeOf(mp).Comparable()
	if !comparable || hedgeInstruments.provider != mp {
		tel, err := newHedgeTelemetry(mp.Meter(tracerName))
		if err != nil {
			tel = nil
		}
		hedgeInstruments.provider, hedgeInstruments.tel = mp, tel
	}
	return hedgeInstruments.tel
}

func newHedgeTelemetry(meter metric.Meter) (*hedgeTelemetry, error) {
	connections, err := meter.Int64Counter("hedge.connections",
		metric.WithDescription("Hedged connections, by the path they took and how First fared"))
	if err != nil {
		return nil, err
	}
	io, err := meter.Int64Counter("hedge.io",
		metric.WithDescription("Bytes carried by hedged connections"),
		metric.WithUnit("By"))
	if err != nil {
		return nil, err
	}
	firstByte, err := meter.Float64Histogram("hedge.time_to_first_byte",
		metric.WithDescription("Time from the hedge's dial to the first byte the client receives"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hedgeFirstByteBuckets...))
	if err != nil {
		return nil, err
	}
	return &hedgeTelemetry{connections: connections, io: io, firstByte: firstByte}, nil
}

// hedgeStats accumulates one connection's record. Its path and outcome can
// change while the connection lives, as it commits or moves; finish records
// it once.
type hedgeStats struct {
	h       *Hedge
	network string
	site    string
	start   time.Time

	mu      sync.Mutex
	path    string
	outcome string
	// sealed is set when the connection closes: what is noted afterwards
	// can be the teardown itself, so it doesn't change the record.
	sealed bool

	firstByte atomic.Int64 // nanoseconds after start, 0 before any
	up, down  atomic.Int64
	once      sync.Once
}

func (h *Hedge) newStats(key hedgeKey, network, path string, start time.Time) *hedgeStats {
	return &hedgeStats{h: h, network: network, site: h.telemetrySite(key), start: start, path: path}
}

// telemetrySite returns the site attribute value: the site when the config
// lists it, "other" when it lists others, "" when it lists none.
func (h *Hedge) telemetrySite(key hedgeKey) string {
	if len(h.telemetrySites) == 0 {
		return ""
	}
	if h.telemetrySites[key.site] {
		return key.site
	}
	return hedgeSiteOther
}

// seal stops later changes to the path and outcome.
func (s *hedgeStats) seal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
}

func (s *hedgeStats) set(path, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return
	}
	if path != "" {
		s.path = path
	}
	if outcome != "" {
		s.outcome = outcome
	}
}

// replaceOutcome sets outcome when the current one is from, or unset.
func (s *hedgeStats) replaceOutcome(from, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return
	}
	if s.outcome == "" || s.outcome == from {
		s.outcome = outcome
	}
}

// setOutcomeIfUnset keeps an earlier, more specific outcome.
func (s *hedgeStats) setOutcomeIfUnset(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sealed && s.outcome == "" {
		s.outcome = outcome
	}
}

func (s *hedgeStats) read(n int) {
	if n <= 0 {
		return
	}
	// Under mu, with finish's snapshot, so a record has the first byte's
	// time and its bytes or neither.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstByte.Load() == 0 {
		s.firstByte.Store(max(int64(time.Since(s.start)), 1))
	}
	s.down.Add(int64(n))
}

func (s *hedgeStats) wrote(n int) {
	if n > 0 {
		s.up.Add(int64(n))
	}
}

// thenDialed records a connection whose dial to Then failed. With no error,
// or for a dial that records nothing (a probe's, or one while the hedge is
// switched off), it does nothing.
func (s *hedgeStats) thenDialed(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	recorded := s.path != ""
	s.mu.Unlock()
	if !recorded {
		return
	}
	s.set("", hedgeOutcomeThenDialError)
	s.finish(true)
}

// finish records the connection. tried reports whether it tried First, so
// an outcome is due.
func (s *hedgeStats) finish(tried bool) {
	s.once.Do(func() {
		t := currentHedgeTelemetry()
		if t == nil {
			return
		}
		s.mu.Lock()
		path, outcome := s.path, s.outcome
		firstByte, up, down := s.firstByte.Load(), s.up.Load(), s.down.Load()
		s.mu.Unlock()
		if tried && outcome == "" {
			outcome = hedgeOutcomeAbandoned
		}
		attrs := []attribute.KeyValue{
			semconv.NetworkProtocolNameKey.String(s.network),
			hedgePathKey.String(path),
			tagAttr(string(hedgeFirstKey), s.h.firstTag),
			tagAttr(string(hedgeThenKey), s.h.thenTag),
		}
		if s.site != "" {
			attrs = append(attrs, hedgeSiteAttrKey.String(s.site))
		}
		pathAttrs := metric.WithAttributeSet(attribute.NewSet(attrs...))
		if outcome != "" {
			attrs = append(attrs, hedgeOutcomeKey.String(outcome))
		}
		ctx := context.Background()
		t.connections.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(attrs...)))
		if firstByte > 0 {
			t.firstByte.Record(ctx, time.Duration(firstByte).Seconds(), pathAttrs)
		}
		dir := func(d string) metric.MeasurementOption {
			return metric.WithAttributeSet(attribute.NewSet(append(attrs[:len(attrs):len(attrs)],
				semconv.NetworkIODirectionKey.String(d))...))
		}
		if up > 0 {
			t.io.Add(ctx, up, dir("transmit"))
		}
		if down > 0 {
			t.io.Add(ctx, down, dir("receive"))
		}
	})
}

// statsStream measures a connection that went straight to Then.
type statsStream struct {
	net.Conn
	stats *hedgeStats
}

func (c *statsStream) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.stats.read(n)
	return n, err
}

func (c *statsStream) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.stats.wrote(n)
	return n, err
}

func (c *statsStream) Close() error {
	c.stats.finish(false)
	return c.Conn.Close()
}

// Upstream lets common.Cast find feature interfaces on the inner conn. It
// does not mark the reader replaceable, so reads still pass through Read.
func (c *statsStream) Upstream() any { return c.Conn }

// newStatsStream wraps conn, keeping half-close exactly as conn has it:
// sing-box's relay half-closes a destination only if it is itself an
// N.WriteCloser, and otherwise closes it at upload EOF, cutting off the
// response.
func newStatsStream(conn net.Conn, stats *hedgeStats) net.Conn {
	c := &statsStream{Conn: conn, stats: stats}
	if wc, ok := conn.(N.WriteCloser); ok {
		return &statsDuplexStream{statsStream: c, closeWriter: wc}
	}
	return c
}

// statsDuplexStream is a statsStream over a conn that can half-close.
type statsDuplexStream struct {
	*statsStream
	closeWriter N.WriteCloser
}

func (c *statsDuplexStream) CloseWrite() error { return c.closeWriter.CloseWrite() }

// statsPacket measures a packet connection that went straight to Then.
type statsPacket struct {
	net.PacketConn
	writer N.NetPacketConn
	stats  *hedgeStats
}

func newStatsPacket(c net.PacketConn, stats *hedgeStats) *statsPacket {
	return &statsPacket{PacketConn: c, writer: netPacketConn(c), stats: stats}
}

func (c *statsPacket) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.stats.read(n)
	}
	return n, addr, err
}

func (c *statsPacket) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.stats.wrote(n)
	return n, err
}

func (c *statsPacket) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	_, addr, err := buffer.ReadPacketFrom(c)
	if err != nil {
		return M.Socksaddr{}, err
	}
	return M.SocksaddrFromNet(addr).Unwrap(), nil
}

// WritePacket keeps domain-name destinations, which WriteTo would strip.
func (c *statsPacket) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	n := buffer.Len()
	err := c.writer.WritePacket(buffer, destination)
	if err == nil {
		c.stats.wrote(n)
	}
	return err
}

func (c *statsPacket) Close() error {
	c.stats.finish(false)
	return c.PacketConn.Close()
}

func (c *statsPacket) Upstream() any { return c.PacketConn }

var (
	_ net.Conn        = (*statsStream)(nil)
	_ N.WriteCloser   = (*statsDuplexStream)(nil)
	_ N.NetPacketConn = (*statsPacket)(nil)
)
