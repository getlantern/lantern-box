//go:build with_quic

package unbounded

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func pathCount(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "unbounded.path" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// unbounded.path follows the global meter provider the embedder installs, including one it
// installs after the first path.
func TestExitPathMetricsFollowAProviderSwap(t *testing.T) {
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	first, second := sdkmetric.NewManualReader(), sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(first)))
	m := &exitPathMetrics{}
	m.path(false)
	assert.EqualValues(t, 1, pathCount(t, first))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(second)))
	m.path(true)
	assert.EqualValues(t, 1, pathCount(t, second), "the path after the swap reaches the new provider")
	assert.EqualValues(t, 1, pathCount(t, first))
}
