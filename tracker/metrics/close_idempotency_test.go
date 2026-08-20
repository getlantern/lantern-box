package metrics

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
	sdkotel "go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/getlantern/geo"
	"github.com/stretchr/testify/require"
)

// Close must be idempotent: sing-box closes a routed conn up to 3x on
// non-graceful teardown, and redundant closes must not drift the metrics.
func TestConnCloseIdempotent(t *testing.T) {
	ctx := context.Background()

	t.Run("Conn", func(t *testing.T) {
		reader := newMeter(t)
		tr := NewTracker(ctx)
		defer tr.Close()
		_, server := net.Pipe()
		conn := tr.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil).(*Conn)
		primeGoodput(&conn.rxBytes, &conn.startTime)
		assertCloseIdempotent(t, reader, conn)
	})

	t.Run("PacketConn", func(t *testing.T) {
		reader := newMeter(t)
		tr := NewTracker(ctx)
		defer tr.Close()
		conn := tr.RoutedPacketConnection(ctx, fakePacketConn{}, adapter.InboundContext{}, nil, nil).(*PacketConn)
		primeGoodput(&conn.rxBytes, &conn.startTime)
		assertCloseIdempotent(t, reader, conn)
	})
}

// assertCloseIdempotent closes conn 3x and asserts every metric the close guard
// covers lands exactly once.
func assertCloseIdempotent(t *testing.T, reader *sdkmetric.ManualReader, conn io.Closer) {
	t.Helper()
	require.Equal(t, int64(1), activeConns(t, reader), "gauge must count the open conn")

	require.NoError(t, conn.Close())
	assertClosedOnce(t, reader)

	for range 2 {
		require.NoError(t, conn.Close())
	}
	assertClosedOnce(t, reader)
}

func assertClosedOnce(t *testing.T, reader *sdkmetric.ManualReader) {
	t.Helper()
	require.Equal(t, int64(0), activeConns(t, reader),
		"active-connection gauge must return to 0 no matter how many times Close is called")
	require.Equal(t, uint64(1), sampleCount(t, reader, "sing.connection_duration"),
		"duration must be recorded once no matter how many times Close is called")
	require.Equal(t, uint64(1), sampleCount(t, reader, "proxy.session.goodput"),
		"goodput must be recorded once no matter how many times Close is called")
}

// primeGoodput backdates a session that moved bytes, since recordGoodput drops
// zero-byte and zero-duration sessions.
func primeGoodput(rxBytes *atomic.Int64, startTime *time.Time) {
	rxBytes.Store(1024)
	*startTime = time.Now().Add(-time.Second)
}

// fakePacketConn is an N.PacketConn that only implements Close.
type fakePacketConn struct{ N.PacketConn }

func (fakePacketConn) Close() error { return nil }

// newMeter points the process-wide provider at an isolated reader and rebuilds
// the package's instruments against it, so tests here must not run in parallel.
func newMeter(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	sdkotel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	SetupMetricsManager(geo.NoLookup{}, "")
	return reader
}

// activeConns returns the summed value of the sing.connections up/down counter.
func activeConns(t *testing.T, r *sdkmetric.ManualReader) int64 {
	t.Helper()
	var total int64
	for _, dp := range collect(t, r, "sing.connections").(metricdata.Sum[int64]).DataPoints {
		total += dp.Value
	}
	return total
}

// sampleCount returns how many samples the named histogram holds.
func sampleCount(t *testing.T, r *sdkmetric.ManualReader, name string) uint64 {
	t.Helper()
	var total uint64
	switch data := collect(t, r, name).(type) {
	case metricdata.Histogram[int64]:
		for _, dp := range data.DataPoints {
			total += dp.Count
		}
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			total += dp.Count
		}
	default:
		t.Fatalf("%s is not a histogram: %T", name, data)
	}
	return total
}

// collect gathers the named metric, failing the test if it was never recorded.
func collect(t *testing.T, r *sdkmetric.ManualReader, name string) metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, r.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data
			}
		}
	}
	t.Fatalf("%s metric not found", name)
	return nil
}
