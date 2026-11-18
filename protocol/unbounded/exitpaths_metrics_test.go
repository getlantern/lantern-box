//go:build with_quic

package unbounded

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
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

// unbounded.path follows the meter provider the embedder installs, including one it installs
// after the first path, through the counter's lookup, leaving OpenTelemetry's global alone.
func TestExitPathMetricsFollowAProviderSwap(t *testing.T) {
	first, second := sdkmetric.NewManualReader(), sdkmetric.NewManualReader()
	var current metric.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(first))
	m := &exitPathMetrics{lookup: func() metric.MeterProvider { return current }}
	m.path(false)
	assert.EqualValues(t, 1, pathCount(t, first))
	current = sdkmetric.NewMeterProvider(sdkmetric.WithReader(second))
	m.path(true)
	assert.EqualValues(t, 1, pathCount(t, second), "the path after the swap reaches the new provider")
	assert.EqualValues(t, 1, pathCount(t, first))
}

// incomparableProvider is a provider whose type is comparable but whose value isn't.
type incomparableProvider struct {
	noop.MeterProvider
	v any
}

func TestExitPathMetricsTakeAnIncomparableProvider(t *testing.T) {
	m := &exitPathMetrics{lookup: func() metric.MeterProvider { return incomparableProvider{v: []int{1}} }}
	assert.NotPanics(t, func() {
		m.path(false)
		m.path(true)
	})
}
