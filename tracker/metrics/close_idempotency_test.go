package metrics

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
	sdkotel "go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/getlantern/geo"
	"github.com/stretchr/testify/require"
)

// Close must be idempotent: sing-box closes a routed conn up to 3x on
// non-graceful teardown, and redundant closes must not drift the gauge.
func TestConnCloseIdempotent(t *testing.T) {
	ctx := context.Background()

	t.Run("Conn", func(t *testing.T) {
		reader := newMeter(t)
		tr := NewTracker(ctx)
		defer tr.Close()
		_, server := net.Pipe()
		assertCloseIdempotent(t, reader,
			tr.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)) // gauge +1
	})

	t.Run("PacketConn", func(t *testing.T) {
		reader := newMeter(t)
		tr := NewTracker(ctx)
		defer tr.Close()
		assertCloseIdempotent(t, reader,
			tr.RoutedPacketConnection(ctx, fakePacketConn{}, adapter.InboundContext{}, nil, nil)) // gauge +1
	})
}

// assertCloseIdempotent closes conn 3x and asserts the gauge returns to 0.
func assertCloseIdempotent(t *testing.T, reader *sdkmetric.ManualReader, conn io.Closer) {
	t.Helper()
	for range 3 {
		require.NoError(t, conn.Close()) // gauge -1 on the first call only
	}
	require.Equal(t, int64(0), activeConns(t, reader),
		"active-connection gauge must return to 0 no matter how many times Close is called")
}

// fakePacketConn is an N.PacketConn whose Close is a no-op (only Close is used).
type fakePacketConn struct{ N.PacketConn }

func (fakePacketConn) Close() error { return nil }

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
	var rm metricdata.ResourceMetrics
	require.NoError(t, r.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "sing.connections" {
				continue
			}
			var total int64
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	t.Fatal("sing.connections metric not found")
	return 0
}
