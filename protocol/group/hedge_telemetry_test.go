package group

import (
	"context"
	"errors"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	lAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

// recordMetrics installs an in-memory meter provider for the test.
func recordMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		_ = mp.Shutdown(context.Background())
		otel.SetMeterProvider(prev)
	})
	return reader
}

type hedgePoint struct {
	attrs  map[string]string
	int    int64
	count  uint64    // histogram sample count
	bounds []float64 // histogram bucket boundaries
}

func collectHedge(t *testing.T, reader *sdkmetric.ManualReader, name string) []hedgePoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var points []hedgePoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					points = append(points, hedgePoint{attrs: attrMap(dp.Attributes.ToSlice()), int: dp.Value})
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					points = append(points, hedgePoint{attrs: attrMap(dp.Attributes.ToSlice()), count: dp.Count, bounds: dp.Bounds})
				}
			}
		}
	}
	return points
}

// connectionRecord returns the single hedge.connections point.
func connectionRecord(t *testing.T, reader *sdkmetric.ManualReader) map[string]string {
	t.Helper()
	points := collectHedge(t, reader, "hedge.connections")
	require.Len(t, points, 1, "want exactly one connection record, got %+v", points)
	require.EqualValues(t, 1, points[0].int)
	return points[0].attrs
}

func attrMap(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func TestHedgeTelemetryOutcomes(t *testing.T) {
	commitThenStall := func(t *testing.T, f *hedgeFixture, c net.Conn) {
		require.Equal(t, "first:hi", exchange(t, c, "hi"))
		s := c.(*hedgeStream)
		s.firstFailed(lAdapter.UserFailureStall, s.move)
	}
	for _, tc := range []struct {
		name    string
		first   *fakeOutbound
		mutate  func(*option.HedgeOutboundOptions)
		use     func(t *testing.T, f *hedgeFixture, c net.Conn)
		path    string
		outcome string
	}{
		{"committed", &fakeOutbound{stream: answer("first:")}, nil, nil, hedgePathFirst, hedgeOutcomeCommitted},
		{"moved on no reply", &fakeOutbound{stream: blackhole}, nil, nil, hedgePathThenAfterMove, hedgeOutcomeMovedNoReply},
		{"moved on reset", &fakeOutbound{stream: reset}, nil, nil, hedgePathThenAfterMove, hedgeOutcomeMovedReset},
		{"moved on dial error", &fakeOutbound{dialErr: errors.New("no strategy")}, nil, nil, hedgePathThenAfterMove, hedgeOutcomeMovedDialError},
		{"stalled after commit", &fakeOutbound{stream: answer("first:")}, nil, commitThenStall, hedgePathFirst, hedgeOutcomeStalledAfterCommit},
		{"overflow", &fakeOutbound{stream: blackhole},
			func(o *option.HedgeOutboundOptions) { o.MaxHeldBytes = 1 },
			func(t *testing.T, f *hedgeFixture, c net.Conn) {
				_, _ = c.Write([]byte(tlsRecord("too long")))
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Read(make([]byte, 16))
			}, hedgePathFirst, hedgeOutcomeOverflowFailed},
		{"abandoned", &fakeOutbound{stream: blackhole},
			func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) },
			func(t *testing.T, f *hedgeFixture, c net.Conn) { _, _ = c.Write([]byte("hi")) },
			hedgePathFirst, hedgeOutcomeAbandoned},
		{"reset, not replayable", &fakeOutbound{stream: reset}, nil,
			func(t *testing.T, f *hedgeFixture, c net.Conn) {
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Write([]byte("hello"))
				_, _ = c.Read(make([]byte, 16))
			}, hedgePathFirst, hedgeOutcomeResetNotReplayable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := recordMetrics(t)
			f := newHedgeFixture(t, tc.first, &fakeOutbound{stream: answer("then:")}, tc.mutate)
			c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
			require.NoError(t, err)
			if tc.use != nil {
				tc.use(t, f, c)
			} else {
				// A handshake opening, the only kind a hedge moves.
				exchange(t, c, tlsRecord("hi"))
			}
			c.Close()
			attrs := connectionRecord(t, reader)
			require.Equal(t, tc.path, attrs["hedge.path"])
			require.Equal(t, tc.outcome, attrs["hedge.outcome"])
			require.Equal(t, "tcp", attrs["network.protocol.name"])
			require.Equal(t, "first", attrs["hedge.first"])
			require.Equal(t, "then", attrs["hedge.then"])
			require.NotContains(t, attrs, "hedge.site", "no site without telemetry_sites")
		})
	}
}

func TestHedgeTelemetryBytesAndFirstByte(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "first:hello", exchange(t, c, "hello"))
	c.Close()

	io := map[string]int64{}
	for _, p := range collectHedge(t, reader, "hedge.io") {
		require.Equal(t, hedgeOutcomeCommitted, p.attrs["hedge.outcome"])
		io[p.attrs["network.io.direction"]] = p.int
	}
	require.Equal(t, map[string]int64{"transmit": 5, "receive": 11}, io)

	ttfb := collectHedge(t, reader, "hedge.time_to_first_byte")
	require.Len(t, ttfb, 1)
	require.EqualValues(t, 1, ttfb[0].count)
	require.Equal(t, hedgePathFirst, ttfb[0].attrs["hedge.path"])
	require.NotContains(t, ttfb[0].attrs, "hedge.outcome", "first byte is keyed by path alone")
	require.Equal(t, hedgeFirstByteBuckets, ttfb[0].bounds, "buckets in seconds, not the SDK's millisecond-sized defaults")
}

// Then's dial failures are counted, as First's are, so the paths compare
// fairly.
func TestHedgeTelemetryThenDialError(t *testing.T) {
	failing := &fakeOutbound{dialErr: errors.New("then down")}
	t.Run("then directly", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, failing, nil)
		f.hedge.cache.mark(hedgeKeyFor(context.Background(), "tcp", hedgeDest), hedgeBad, time.Minute)
		_, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
		require.Error(t, err)
		attrs := connectionRecord(t, reader)
		require.Equal(t, hedgePathThenKnownBad, attrs["hedge.path"])
		require.Equal(t, hedgeOutcomeThenDialError, attrs["hedge.outcome"])
	})
	t.Run("after first failed to dial", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{dialErr: errors.New("no strategy")}, failing, nil)
		_, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
		require.Error(t, err)
		attrs := connectionRecord(t, reader)
		require.Equal(t, hedgePathFirst, attrs["hedge.path"], "no move happened")
		require.Equal(t, hedgeOutcomeThenDialError, attrs["hedge.outcome"])
		require.Equal(t, "udp", attrs["network.protocol.name"])
	})
	t.Run("a probe records nothing", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, failing, nil)
		_, err := f.hedge.DialContext(lAdapter.ContextWithProbe(f.ctx), "tcp", hedgeDest)
		require.Error(t, err)
		require.Empty(t, collectHedge(t, reader, "hedge.connections"))
	})
}

// A move to Then that fails keeps the first path and records move_failed,
// so it doesn't look like a successful move.
func TestHedgeTelemetryMoveFailed(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{dialErr: errors.New("then down")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Write([]byte(tlsRecord("hi")))
	require.NoError(t, err)
	_, err = c.Read(make([]byte, 16))
	require.Error(t, err, "the move fails")
	c.Close()
	attrs := connectionRecord(t, reader)
	require.Equal(t, hedgePathFirst, attrs["hedge.path"])
	require.Equal(t, hedgeOutcomeMoveFailed, attrs["hedge.outcome"])
}

// recordingHalfCloser is a conn that can half-close, and records that it did.
type recordingHalfCloser struct {
	net.Conn
	closedWrite bool
}

func (c *recordingHalfCloser) CloseWrite() error { c.closedWrite = true; return nil }

// The relay half-closes a destination only if it is an N.WriteCloser, so
// wrapping Then's conn must keep that exactly as the conn has it.
func TestStatsStreamKeepsHalfClose(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	hc := &recordingHalfCloser{Conn: a}
	wrapped, ok := newStatsStream(hc, &hedgeStats{}).(N.WriteCloser)
	require.True(t, ok, "a conn that can half-close stays one")
	require.NoError(t, wrapped.CloseWrite())
	require.True(t, hc.closedWrite)

	_, ok = newStatsStream(a, &hedgeStats{}).(N.WriteCloser)
	require.False(t, ok, "one that can't doesn't pretend to")
}

// A caller that gives up while First dials is counted as abandoned, not
// left out of the comparison.
func TestHedgeTelemetryCancelledDialIsAbandoned(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{dialErr: errors.New("cancelled")}, &fakeOutbound{stream: answer("then:")}, nil)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err := f.hedge.DialContext(ctx, "tcp", hedgeDest)
	require.Error(t, err)
	attrs := connectionRecord(t, reader)
	require.Equal(t, hedgePathFirst, attrs["hedge.path"])
	require.Equal(t, hedgeOutcomeAbandoned, attrs["hedge.outcome"])
}

// A reset the watchdog charges is recorded before its handling, which runs
// on its own goroutine and can come after the client closed.
func TestHedgeTelemetryResetNotedBeforeItIsHandled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*option.HedgeOutboundOptions)
		write   string
		read    bool
		outcome string
	}{
		{"after commit", nil, tlsRecord("hi"), true, hedgeOutcomeResetAfterCommit},
		{"not replayable", nil, "hello", false, hedgeOutcomeResetNotReplayable},
		{"overflowed", func(o *option.HedgeOutboundOptions) { o.MaxHeldBytes = 1 }, tlsRecord("too long"), false, hedgeOutcomeOverflowFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := recordMetrics(t)
			first := &fakeOutbound{stream: answer("first:")}
			if !tc.read {
				first = &fakeOutbound{stream: blackhole}
			}
			f := newHedgeFixture(t, first, &fakeOutbound{stream: answer("then:")}, tc.mutate)
			c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
			require.NoError(t, err)
			require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = c.Write([]byte(tc.write))
			require.NoError(t, err)
			if tc.read {
				_, err = c.Read(make([]byte, 64))
				require.NoError(t, err)
			}
			c.(*hedgeStream).noteReset() // as the watchdog does, before firstFailed runs
			c.Close()
			require.Equal(t, tc.outcome, connectionRecord(t, reader)["hedge.outcome"])
			require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()),
				"the site is marked bad though the client closed before firstFailed ran")
		})
	}
}

// A reply that lands after a reset was noted doesn't turn the record into
// a commit: the reset came first.
func TestHedgeTelemetryCommitKeepsAnEarlierReset(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Write([]byte("hello"))
	require.NoError(t, err)
	c.(*hedgeStream).noteReset()
	_, err = c.Read(make([]byte, 64))
	require.NoError(t, err, "First's reply commits the connection")
	c.Close()
	require.Equal(t, hedgeOutcomeResetNotReplayable, connectionRecord(t, reader)["hedge.outcome"])
	require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()), "the reply doesn't mark the site good again")
}

// A reset on a replayable opening that a reply then commits: the record and
// the cache agree that First failed this connection before replying.
func TestHedgeTelemetryResetAndReplyAgree(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = c.Write([]byte(tlsRecord("hi")))
	require.NoError(t, err)
	s := c.(*hedgeStream)
	s.noteReset()
	_, err = c.Read(make([]byte, 64))
	require.NoError(t, err)
	require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()), "the reply doesn't undo the reset")
	s.firstFailed(lAdapter.UserFailureReset, s.move)
	c.Close()
	require.Equal(t, hedgeOutcomeResetUnmoved, connectionRecord(t, reader)["hedge.outcome"],
		"a reset before the reply isn't relabelled as one after it, nor as a move")
	require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()))
}

// What arrives after the client closed, a failure (which can be the
// teardown itself) or a reply, changes neither the record nor the cache.
func TestHedgeLateEventsAfterCloseChangeNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(s *hedgeStream)
	}{
		{"reset", func(s *hedgeStream) { s.noteReset() }},
		{"failure", func(s *hedgeStream) { s.firstFailed(lAdapter.UserFailureReset, s.move) }},
		{"reply", func(s *hedgeStream) { s.commit(s.releaseLocked) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := recordMetrics(t)
			f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")},
				func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
			c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
			require.NoError(t, err)
			_, err = c.Write([]byte(tlsRecord("hi")))
			require.NoError(t, err)
			c.Close()
			tc.event(c.(*hedgeStream))
			require.Equal(t, hedgeOutcomeAbandoned, connectionRecord(t, reader)["hedge.outcome"])
			require.Equal(t, hedgeUnknown, f.verdict(hedgeDest.String()))
		})
	}
}

// The same, in the window where Close has marked the leg closed but not yet
// written the record.
func TestHedgeEventsBetweenCloseAndRecordChangeNothing(t *testing.T) {
	for _, opening := range []string{tlsRecord("hi"), "hello"} {
		t.Run(opening[3:], func(t *testing.T) {
			reader := recordMetrics(t)
			f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")},
				func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
			c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
			require.NoError(t, err)
			_, err = c.Write([]byte(opening))
			require.NoError(t, err)
			s := c.(*hedgeStream)
			s.mu.Lock()
			s.closeLocked() // Close's first step
			s.mu.Unlock()
			s.noteReset()
			c.Close() // records
			require.Equal(t, hedgeOutcomeAbandoned, connectionRecord(t, reader)["hedge.outcome"])
			require.Equal(t, hedgeUnknown, f.verdict(hedgeDest.String()))
		})
	}
}

// A reset on an opening not yet long enough to classify isn't recorded as
// not replayable: it may still become a handshake and move.
func TestHedgeResetOnAnUndecidedOpening(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	_, err = c.Write([]byte("\x16"))
	require.NoError(t, err)
	c.(*hedgeStream).noteReset()
	c.Close()
	require.Equal(t, hedgeOutcomeResetUnmoved, connectionRecord(t, reader)["hedge.outcome"])
}

// A reset on a replayable opening whose client closes before the move
// starts still shows as a reset, in the record and the cache.
func TestHedgeResetBeforeTheMoveShows(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.CommitTimeout = badoption.Duration(time.Minute) })
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	_, err = c.Write([]byte(tlsRecord("hi")))
	require.NoError(t, err)
	c.(*hedgeStream).noteReset()
	c.Close()
	attrs := connectionRecord(t, reader)
	require.Equal(t, hedgePathFirst, attrs["hedge.path"])
	require.Equal(t, hedgeOutcomeResetUnmoved, attrs["hedge.outcome"])
	require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()))
}

// A committed connection whose failure arrives after the close keeps its
// record and its good verdict.
func TestHedgeCommittedFailureAfterCloseChangesNothing(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "first:hi", exchange(t, c, "hi"))
	s := c.(*hedgeStream)
	s.mu.Lock()
	s.closeLocked() // Close's first step
	s.mu.Unlock()
	s.firstFailed(lAdapter.UserFailureReset, s.move)
	s.noteReset()
	c.Close()
	require.Equal(t, hedgeOutcomeCommitted, connectionRecord(t, reader)["hedge.outcome"])
	require.Equal(t, hedgeGood, f.verdict(hedgeDest.String()))
}

// A real reset on First, through the watchdog's hook: a write that isn't a
// handshake fails, the client closes at once, and the record and the cache
// still show it. firstFailed can also get there first, so this checks the
// wiring end to end; TestHedgeTelemetryResetNotedBeforeItIsHandled pins the
// ordering.
func TestHedgeTelemetryResetOnWriteEndToEnd(t *testing.T) {
	reader := recordMetrics(t)
	first := &fakeOutbound{stream: blackhole, wrap: func(c net.Conn) net.Conn {
		return &failingConn{Conn: c, err: connResetErr(), failWrite: true}
	}}
	f := newHedgeFixture(t, first, &fakeOutbound{stream: answer("then:")}, nil)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	_, err = c.Write([]byte("hello"))
	require.Error(t, err)
	c.Close()
	require.Equal(t, hedgeOutcomeResetNotReplayable, connectionRecord(t, reader)["hedge.outcome"])
	require.Equal(t, hedgeBad, f.verdict(hedgeDest.String()))
}

// A hedge built before the meter provider is replaced, as radiance does when
// telemetry's config changes, records to the new one.
func TestHedgeTelemetryFollowsTheMeterProvider(t *testing.T) {
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	reader := recordMetrics(t)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "first:hi", exchange(t, c, "hi"))
	c.Close()
	require.Equal(t, hedgePathFirst, connectionRecord(t, reader)["hedge.path"])
}

func TestHedgeTelemetryThenPaths(t *testing.T) {
	t.Run("known bad", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
		f.hedge.cache.mark(hedgeKeyFor(context.Background(), "tcp", hedgeDest), hedgeBad, time.Minute)
		c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
		require.NoError(t, err)
		require.Equal(t, "then:x", exchange(t, c, "x"))
		c.Close()
		attrs := connectionRecord(t, reader)
		require.Equal(t, hedgePathThenKnownBad, attrs["hedge.path"])
		require.NotContains(t, attrs, "hedge.outcome", "connections that never tried First have no outcome")
	})
	t.Run("first lacks the network", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{networks: []string{"tcp"}}, &fakeOutbound{packet: echoPacket("then:")}, nil)
		pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
		require.NoError(t, err)
		_, err = pc.WriteTo([]byte("q"), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
		require.NoError(t, err)
		require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, _, err = pc.ReadFrom(make([]byte, 16))
		require.NoError(t, err)
		pc.Close()
		attrs := connectionRecord(t, reader)
		require.Equal(t, hedgePathThen, attrs["hedge.path"])
		require.Equal(t, "udp", attrs["network.protocol.name"])
	})
	t.Run("udp moved", func(t *testing.T) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{}, &fakeOutbound{packet: echoPacket("then:")}, nil)
		pc, err := f.hedge.ListenPacket(f.ctx, hedgeDest)
		require.NoError(t, err)
		_, err = pc.WriteTo([]byte(quicPacket("initial")), &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443})
		require.NoError(t, err)
		require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, _, err = pc.ReadFrom(make([]byte, 64))
		require.NoError(t, err)
		pc.Close()
		attrs := connectionRecord(t, reader)
		require.Equal(t, hedgePathThenAfterMove, attrs["hedge.path"])
		require.Equal(t, hedgeOutcomeMovedNoReply, attrs["hedge.outcome"])
	})
}

func TestHedgeTelemetryProbesRecordNothing(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	probe := lAdapter.ContextWithProbe(f.ctx)
	c, err := f.hedge.DialContext(probe, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "then:probe", exchange(t, c, "probe"))
	c.Close()
	pc, err := f.hedge.ListenPacket(probe, hedgeDest)
	require.NoError(t, err)
	pc.Close()
	for _, name := range []string{"hedge.connections", "hedge.io", "hedge.time_to_first_byte"} {
		require.Empty(t, collectHedge(t, reader, name), "a probe must record no %s", name)
	}
}

// A switched-off hedge sends everything to Then, and recording it would
// report every connection of every client while the feature is off.
func TestHedgeTelemetrySwitchedOffRecordsNothing(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	f.hedge.SetEnabled(false)
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "then:off", exchange(t, c, "off"))
	c.Close()
	require.Empty(t, collectHedge(t, reader, "hedge.connections"))

	f.hedge.SetEnabled(true)
	c, err = f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	require.Equal(t, "first:on", exchange(t, c, "on"))
	c.Close()
	require.Equal(t, hedgePathFirst, connectionRecord(t, reader)["hedge.path"])
}

func TestHedgeTelemetrySites(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")},
		func(o *option.HedgeOutboundOptions) { o.TelemetrySites = []string{"www.YouTube.com."} })
	dial := func(host string) {
		c, err := f.hedge.DialContext(f.ctx, "tcp", M.ParseSocksaddr(host))
		require.NoError(t, err)
		exchange(t, c, "hi")
		c.Close()
	}
	dial("rr1---sn-x.youtube.com:443")
	require.Equal(t, "youtube.com", connectionRecord(t, reader)["hedge.site"], "a listed site is named")
	dial("private-blog.example:443")
	sites := map[string]int64{}
	for _, p := range collectHedge(t, reader, "hedge.connections") {
		sites[p.attrs["hedge.site"]] += p.int
	}
	require.Equal(t, map[string]int64{"youtube.com": 1, "other": 1}, sites, "every other site is \"other\"")
}

func TestHedgeTelemetryRedactsHelperTags(t *testing.T) {
	reader := recordMetrics(t)
	f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")}, nil)
	f.hedge.thenTag = helperTagA
	f.mgr.outbounds[helperTagA] = f.then
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	exchange(t, c, "hi")
	c.Close()
	for _, v := range connectionRecord(t, reader) {
		require.False(t, strings.Contains(v, "0123456789ab"), "helper tag exported: %s", v)
	}
}

func TestHedgeExplore(t *testing.T) {
	newExploring := func(t *testing.T, rate float64, draws []float64) (*hedgeFixture, *sdkmetric.ManualReader) {
		reader := recordMetrics(t)
		f := newHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")},
			func(o *option.HedgeOutboundOptions) { o.ExploreRate = rate })
		f.hedge.random = func() float64 {
			d := draws[0]
			draws = draws[1:]
			return d
		}
		return f, reader
	}
	dial := func(t *testing.T, f *hedgeFixture) {
		c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
		require.NoError(t, err)
		exchange(t, c, "hi")
		c.Close()
	}
	paths := func(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
		got := map[string]int64{}
		for _, p := range collectHedge(t, reader, "hedge.connections") {
			got[p.attrs["hedge.path"]] += p.int
		}
		return got
	}

	t.Run("zero never explores", func(t *testing.T) {
		f, reader := newExploring(t, 0, nil) // random must not be consulted
		for range 3 {
			dial(t, f)
		}
		require.Equal(t, map[string]int64{hedgePathFirst: 3}, paths(t, reader))
	})
	t.Run("one always explores", func(t *testing.T) {
		f, reader := newExploring(t, 1, []float64{0, 0.5, 0.999})
		for range 3 {
			dial(t, f)
		}
		require.Equal(t, map[string]int64{hedgePathThenExplore: 3}, paths(t, reader))
		require.Zero(t, f.first.dials.Load())
		require.Equal(t, hedgeUnknown, f.verdict("www.youtube.com:443"), "exploring teaches the cache nothing")
	})
	t.Run("fraction", func(t *testing.T) {
		f, reader := newExploring(t, 0.25, []float64{0.1, 0.9, 0.3, 0.2})
		for range 4 {
			dial(t, f)
		}
		require.Equal(t, map[string]int64{hedgePathThenExplore: 2, hedgePathFirst: 2}, paths(t, reader))
		require.EqualValues(t, 2, f.first.dials.Load())
	})
	t.Run("probes neither explore nor draw", func(t *testing.T) {
		f, reader := newExploring(t, 1, nil)
		c, err := f.hedge.DialContext(lAdapter.ContextWithProbe(f.ctx), "tcp", hedgeDest)
		require.NoError(t, err)
		c.Close()
		require.Empty(t, paths(t, reader))
	})
	t.Run("known-bad sites don't explore", func(t *testing.T) {
		f, reader := newExploring(t, 1, nil)
		f.hedge.cache.mark(hedgeKeyFor(context.Background(), "tcp", hedgeDest), hedgeBad, time.Minute)
		dial(t, f)
		require.Equal(t, map[string]int64{hedgePathThenKnownBad: 1}, paths(t, reader))
	})
}

func TestNewHedgeRejectsBadExploreRate(t *testing.T) {
	ctx := service.ContextWithDefaultRegistry(context.Background())
	for _, rate := range []float64{-0.1, 1.5, math.NaN()} {
		_, err := NewHedge(ctx, nil, log.NewNOPFactory().Logger(), "h",
			option.HedgeOutboundOptions{First: "a", Then: "b", ExploreRate: rate})
		require.Error(t, err, "rate %v", rate)
	}
}
