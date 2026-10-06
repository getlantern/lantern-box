package metrics

import (
	"context"
	"net"
	"testing"
	"testing/synctest"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeGeo resolves every IP to one country, ASN and ISP.
type fakeGeo struct{}

func (fakeGeo) CountryCode(net.IP) string { return "IR" }
func (fakeGeo) ASN(net.IP) string         { return "AS197207" }
func (fakeGeo) ISP(net.IP) string         { return "Mobile Communication Company of Iran" }

// allPointAttrs returns the attributes of every data point of metric name.
func allPointAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string) []map[string]any {
	t.Helper()
	var sets []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			}
		}
	}
	require.NotEmpty(t, sets, "metric %s not recorded", name)
	out := make([]map[string]any, 0, len(sets))
	for _, set := range sets {
		attrs := map[string]any{}
		for _, kv := range set.ToSlice() {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		out = append(out, attrs)
	}
	return out
}

// pointAttrs returns the attributes of the first data point of metric name.
func pointAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]any {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			out := map[string]any{}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				require.NotEmpty(t, d.DataPoints, name)
				for _, kv := range d.DataPoints[0].Attributes.ToSlice() {
					out[string(kv.Key)] = kv.Value.AsInterface()
				}
			case metricdata.Histogram[int64]:
				require.NotEmpty(t, d.DataPoints, name)
				for _, kv := range d.DataPoints[0].Attributes.ToSlice() {
					out[string(kv.Key)] = kv.Value.AsInterface()
				}
			}
			return out
		}
	}
	t.Fatalf("metric %s not recorded", name)
	return nil
}

func TestProxyIOCarriesASNAndISP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := metric.NewManualReader()
		sdkotel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))

		SetISPLookup(fakeGeo{})
		SetupMetricsManager(fakeGeo{}, "")
		// The lookup workers would outlive the bubble, which synctest reports
		// as a deadlock, and later tests would inherit the lookups.
		defer func() {
			close(metrics.countryLookupC)
			metrics.countryLookupC = nil
			metrics.countryLookup = geo.NoLookup{}
			metrics.ispLookup = geo.NoLookup{}
		}()

		ctx := context.Background()
		tracker := NewTracker(ctx)
		defer tracker.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		tracked := tracker.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)
		// Let the workers resolve the client before any bytes are counted.
		synctest.Wait()

		go func() {
			buf := make([]byte, 16)
			_, _ = tracked.Read(buf)
		}()
		_, _ = client.Write([]byte("hello"))
		synctest.Wait()
		tracked.Close()
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))

		io := pointAttrs(t, rm, "proxy.io")
		assert.Equal(t, "IR", io["geo.country.iso_code"])
		assert.Equal(t, "AS197207", io["client.asn"])
		assert.Equal(t, "Mobile Communication Company of Iran", io["client.isp"])

		// Every ASN multiplies a series, so the split stays off the other
		// metrics. (Their first point can predate the lookup, so only the
		// absence of the keys is checked, on every point.)
		for _, name := range []string{"sing.connections", "sing.connection_duration"} {
			for _, attrs := range allPointAttrs(t, rm, name) {
				assert.Nil(t, attrs["client.asn"], "%s should not have client.asn", name)
				assert.Nil(t, attrs["client.isp"], "%s should not have client.isp", name)
			}
		}
	})
}

func TestProxyIOWithoutISPLookupHasNoASN(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := metric.NewManualReader()
		sdkotel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))

		SetupMetricsManager(geo.NoLookup{}, "")

		ctx := context.Background()
		tracker := NewTracker(ctx)
		defer tracker.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		tracked := tracker.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)
		go func() {
			buf := make([]byte, 16)
			_, _ = tracked.Read(buf)
		}()
		_, _ = client.Write([]byte("hello"))
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))

		io := pointAttrs(t, rm, "proxy.io")
		assert.NotNil(t, io["network.io.direction"], "proxy.io was recorded")
		assert.Nil(t, io["client.asn"], "without the ISP database there is no client.asn series")
		assert.Nil(t, io["client.isp"], "without the ISP database there is no client.isp series")
	})
}
