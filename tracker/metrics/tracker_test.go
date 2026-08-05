//go:build goexperiment.synctest

package metrics

import (
	"context"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// infoConn stages a ClientInfo on a connection so clientcontext.InfoFromConn
// resolves it, standing in for a Manager-wrapped conn.
type infoConn struct {
	net.Conn
	info clientcontext.ClientInfo
}

func (c infoConn) ClientInfo() (clientcontext.ClientInfo, bool) { return c.info, true }

func TestTracker(t *testing.T) {
	synctest.Run(func() {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{}, "")

		ctx := context.Background()
		metricsTracker := NewTracker(ctx)
		defer metricsTracker.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		serverTracked := metricsTracker.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

		clientSentMessage := []byte("A client sent a short request...")
		serverReceive := 0
		go func() {
			buf := make([]byte, len(clientSentMessage))
			var err error
			serverReceive, err = serverTracked.Read(buf)
			assert.NoError(t, err)
		}()

		_, err := client.Write(clientSentMessage)
		assert.NoError(t, err)

		serverSentMessage := []byte("...and the server sent a short response.")
		go func() {
			buf := make([]byte, len(serverSentMessage))
			_, err := client.Read(buf)
			assert.NoError(t, err)
		}()

		serverTransmit, err := serverTracked.Write(serverSentMessage)
		assert.NoError(t, err)

		synctest.Wait()

		var rm metricdata.ResourceMetrics
		reader.Collect(ctx, &rm)

		ioCounter := extractCountersByAttribute(rm, "proxy.io")
		results := map[string]int64{}
		for k, v := range ioCounter {
			if strings.Contains(k, "direction=transmit") {
				results["transmit"] += v
			} else if strings.Contains(k, "direction=receive") {
				results["receive"] += v
			}
		}
		assert.Equal(t, int64(serverTransmit), results["transmit"], "transmit bytes did not match")
		assert.Equal(t, int64(serverReceive), results["receive"], "receive bytes did not match")
	})
}

func TestTrackerWithClientInfo(t *testing.T) {
	synctest.Run(func() {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{}, "")

		info := clientcontext.ClientInfo{
			DeviceID: "dev-42",
			Platform: "android",
			IsPro:    true,
			Version:  "7.0",
		}
		tracker := NewTracker(context.Background())
		defer tracker.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		tracked := tracker.RoutedConnection(
			context.Background(), infoConn{Conn: server, info: info}, adapter.InboundContext{}, nil, nil,
		)

		// Exchange some bytes so proxy.io fires.
		go func() {
			buf := make([]byte, 16)
			_, _ = tracked.Read(buf)
		}()
		_, _ = client.Write([]byte("hello"))
		synctest.Wait()

		// Close triggers Leave → duration + conns-1.
		tracked.Close()
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		reader.Collect(context.Background(), &rm)

		// All metrics carry low-cardinality client attrs.
		for _, name := range []string{
			"proxy.io",
			"sing.connections",
			"sing.connection_duration",
		} {
			attrs := extractAttrs(rm, name)
			assert.Equal(t, "android",
				attrs["client.platform"],
				"%s: platform", name)
			assert.Equal(t, true,
				attrs["client.is_pro"],
				"%s: is_pro", name)
			assert.Equal(t, "7.0",
				attrs["client.version"],
				"%s: version", name)
		}

		// device_id only on proxy.io (high-cardinality).
		ioAttrs := extractAttrs(rm, "proxy.io")
		assert.Equal(t, "dev-42", ioAttrs["client.device_id"])

		connAttrs := extractAttrs(rm, "sing.connections")
		assert.Nil(t, connAttrs["client.device_id"],
			"sing.connections should not have device_id")

		durAttrs := extractAttrs(rm, "sing.connection_duration")
		assert.Nil(t, durAttrs["client.device_id"],
			"sing.connection_duration should not have device_id")
	})
}

func TestDeviceConnectedSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	prevTP := sdkotel.GetTracerProvider()
	sdkotel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		sdkotel.SetTracerProvider(prevTP)
	})

	info := clientcontext.ClientInfo{
		DeviceID:    "test-device-123",
		Platform:    "android",
		IsPro:       true,
		CountryCode: "CA",
		Version:     "10.0",
	}
	emitDeviceConnectedSpan(context.Background(), info)

	spans := exporter.GetSpans()
	var deviceSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "device_id.connected" {
			deviceSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, deviceSpan,
		"device_id.connected span should be emitted")

	attrs := make(map[string]any)
	for _, attr := range deviceSpan.Attributes {
		attrs[string(attr.Key)] = attr.Value.AsInterface()
	}
	assert.Equal(t, "test-device-123", attrs["client.device_id"])
	assert.Equal(t, "android", attrs["client.platform"])
	assert.Equal(t, true, attrs["client.is_pro"])
	assert.Equal(t, "CA", attrs["geo.country.iso_code"])
	assert.Equal(t, "10.0", attrs["client.version"])
}

// TestSessionGoodput verifies the per-session download goodput histogram is
// emitted once at close, with the value ~= received bytes / connection seconds
// and — crucially for the bandit evaluator — the three queryable POINT
// attributes it filters/groups by: network.io.direction='receive', the bare
// "track" key (NOT the "proxy.track" resource attr), and geo.country.iso_code.
func TestSessionGoodput(t *testing.T) {
	synctest.Run(func() {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{}, "challenger-01")

		ctx := context.Background()
		mt := NewTracker(ctx)
		defer mt.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		serverTracked := mt.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

		const n = 1_100_000
		done := make(chan int, 1)
		go func() {
			buf := make([]byte, n)
			got := 0
			for got < n {
				r, err := serverTracked.Read(buf[got:])
				if err != nil {
					break
				}
				got += r
			}
			done <- got
		}()
		_, err := client.Write(make([]byte, n))
		require.NoError(t, err)
		require.Equal(t, n, <-done)

		// Advance virtual time so the connection has a ~1s open duration.
		time.Sleep(time.Second)
		synctest.Wait()

		require.NoError(t, serverTracked.Close())
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		reader.Collect(ctx, &rm)

		count, sum, found := histogramCountSum(rm, "proxy.session.goodput")
		require.True(t, found, "goodput histogram should be emitted")
		assert.Equal(t, uint64(1), count, "exactly one goodput sample")
		// ~1s open duration → goodput ~= received bytes per second.
		assert.InDelta(t, float64(n), sum, float64(n)*0.05)

		// The evaluator filters `network.io.direction = 'receive'` and
		// `track IN [...]`, and groups by `track, geo.country.iso_code`. All
		// three must be present as POINT attributes on the sample, and track
		// must use the bare "track" key (the "proxy.track" resource attr is not
		// a queryable label). Keys/values verified against lantern-cloud
		// GoodputByStratum (cmd/api/experiment/signoz.go).
		attrs := extractAttrs(rm, "proxy.session.goodput")
		assert.Equal(t, "receive", attrs["network.io.direction"], "direction point attr")
		assert.Equal(t, "challenger-01", attrs["track"], "track point attr (bare 'track' key)")
		assert.Nil(t, attrs["proxy.track"], "goodput must not rely on the proxy.track resource key")
		assert.Equal(t, ccNa, attrs["geo.country.iso_code"], "country point attr")
	})
}

// TestSessionGoodputSmallSession is the core regression test for the byte-floor
// removal: a ~20 KB session (near the real per-session average, ~45× under the
// old 1 MB floor) must now record a goodput sample. Before the fix this emitted
// nothing, which is what blinded the evaluator's goodput axis for the sing-box
// protocol fleet.
func TestSessionGoodputSmallSession(t *testing.T) {
	synctest.Run(func() {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{}, "")

		ctx := context.Background()
		mt := NewTracker(ctx)
		defer mt.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		serverTracked := mt.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

		const n = 20_000 // well under the removed 1 MB floor
		done := make(chan int, 1)
		go func() {
			buf := make([]byte, n)
			got := 0
			for got < n {
				r, err := serverTracked.Read(buf[got:])
				if err != nil {
					break
				}
				got += r
			}
			done <- got
		}()
		_, err := client.Write(make([]byte, n))
		require.NoError(t, err)
		require.Equal(t, n, <-done)

		// ~2s open duration → goodput ~= n/2 bytes/s.
		time.Sleep(2 * time.Second)
		synctest.Wait()

		require.NoError(t, serverTracked.Close())
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		reader.Collect(ctx, &rm)

		count, sum, found := histogramCountSum(rm, "proxy.session.goodput")
		require.True(t, found, "a 20 KB session must record a goodput sample after floor removal")
		assert.Equal(t, uint64(1), count, "exactly one goodput sample")
		assert.InDelta(t, float64(n)/2.0, sum, float64(n)/2.0*0.05)
	})
}

// TestSessionGoodputZeroBytes verifies the rxBytes > 0 guard: a session that
// received no bytes records nothing even though it was open for a while.
func TestSessionGoodputZeroBytes(t *testing.T) {
	synctest.Run(func() {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{}, "")

		ctx := context.Background()
		mt := NewTracker(ctx)
		defer mt.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		serverTracked := mt.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

		// No bytes received; only elapse time so the duration is non-zero.
		time.Sleep(time.Second)
		synctest.Wait()

		require.NoError(t, serverTracked.Close())
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		reader.Collect(ctx, &rm)
		_, _, found := histogramCountSum(rm, "proxy.session.goodput")
		assert.False(t, found, "no goodput sample when zero bytes were received")
	})
}

// TestSessionGoodputZeroDuration verifies the durationMs > 0 guard directly.
// recordGoodput doesn't touch its receiver, so a zero-value tracker is fine;
// this keeps the divide-by-zero guard deterministic (a net.Pipe close can race
// to a sub-millisecond, but never exactly-zero, virtual duration).
func TestSessionGoodputZeroDuration(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	sdkotel.SetMeterProvider(provider)

	SetupMetricsManager(geo.NoLookup{}, "")

	mt := &MetricsTracker{}
	attrs := metadataToAttributes(adapter.InboundContext{})
	mt.recordGoodput(1_000_000, 0, attrs)

	var rm metricdata.ResourceMetrics
	reader.Collect(context.Background(), &rm)
	_, _, found := histogramCountSum(rm, "proxy.session.goodput")
	assert.False(t, found, "no goodput sample for zero duration")
}

func histogramCountSum(rm metricdata.ResourceMetrics, name string) (uint64, float64, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if d, ok := m.Data.(metricdata.Histogram[float64]); ok && len(d.DataPoints) > 0 {
				return d.DataPoints[0].Count, d.DataPoints[0].Sum, true
			}
		}
	}
	return 0, 0, false
}

func extractCountersByAttribute(rm metricdata.ResourceMetrics, name string) map[string]int64 {
	result := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				result[string(dp.Attributes.Encoded(attribute.DefaultEncoder()))] = dp.Value
			}
		}
	}
	return result
}

// extractAttrs collects the attribute key→value pairs from the
// first data point of the named metric, across all aggregation types.
func extractAttrs(rm metricdata.ResourceMetrics, name string) map[string]any {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			var set attribute.Set
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				if len(d.DataPoints) > 0 {
					set = d.DataPoints[0].Attributes
				}
			case metricdata.Histogram[int64]:
				if len(d.DataPoints) > 0 {
					set = d.DataPoints[0].Attributes
				}
			case metricdata.Histogram[float64]:
				if len(d.DataPoints) > 0 {
					set = d.DataPoints[0].Attributes
				}
			default:
				continue
			}
			out := make(map[string]any)
			iter := set.Iter()
			for iter.Next() {
				kv := iter.Attribute()
				out[string(kv.Key)] = kv.Value.AsInterface()
			}
			return out
		}
	}
	return nil
}
