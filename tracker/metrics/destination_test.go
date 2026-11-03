package metrics

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// countryByIP resolves the listed IPs to their countries, and every other IP,
// the client's among them, to IR.
type countryByIP map[string]string

func (c countryByIP) CountryCode(ip net.IP) string {
	if country, ok := c[ip.String()]; ok {
		return country
	}
	return "IR"
}

func TestDestinationCountry(t *testing.T) {
	lookup := countryByIP{"1.2.3.4": "DE", "5.6.7.8": "US"}

	assert.Equal(t, "DE", destinationCountry(lookup, M.ParseSocksaddr("1.2.3.4:443"), nil), "an IP destination is looked up directly")
	assert.Equal(t, "DE", destinationCountry(lookup, M.ParseSocksaddr("[::ffff:1.2.3.4]:443"), nil), "a 4-in-6 address is looked up as IPv4")
	assert.Equal(t, "US", destinationCountry(lookup, M.ParseSocksaddr("example.com:443"), []netip.Addr{netip.MustParseAddr("5.6.7.8")}),
		"a domain routing resolved counts under its first address")
	assert.Empty(t, destinationCountry(lookup, M.ParseSocksaddr("example.com:443"), nil), "an unresolved domain is not looked up")
	assert.Empty(t, destinationCountry(lookup, M.Socksaddr{}, nil), "an empty destination has no country")
}

func TestProxyDestinationIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := metric.NewManualReader()
		sdkotel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))

		SetupMetricsManager(countryByIP{"1.2.3.4": "DE"}, "")
		defer stopLookupWorkers()

		ctx := context.Background()
		tracker := NewTracker(ctx)
		defer tracker.Close()

		for _, metadata := range []adapter.InboundContext{
			{Destination: M.ParseSocksaddr("1.2.3.4:443")},
			// A domain a route "resolve" action resolved before routing.
			{Destination: M.ParseSocksaddr("bank.example.ir:443"), DestinationAddresses: []netip.Addr{netip.MustParseAddr("5.6.7.8")}},
			{Destination: M.ParseSocksaddr("unresolved.example:443")},
		} {
			client, server := net.Pipe()
			tracked := tracker.RoutedConnection(ctx, infoConn{Conn: server, info: testInfo}, metadata, nil, nil)
			// Let the workers resolve the client and destination before any
			// bytes are counted.
			synctest.Wait()

			go func() {
				buf := make([]byte, 16)
				_, _ = tracked.Read(buf)
			}()
			_, _ = client.Write([]byte("hello"))
			synctest.Wait()
			tracked.Close()
			client.Close()
			synctest.Wait()
		}

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))

		bytesByDestination := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "proxy.destination.io" {
					continue
				}
				for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
					attrs := map[string]any{}
					for _, kv := range dp.Attributes.ToSlice() {
						attrs[string(kv.Key)] = kv.Value.AsInterface()
					}
					assert.Len(t, attrs, 3, "only the country pair and direction: %v", attrs)
					assert.Equal(t, "IR", attrs["geo.country.iso_code"])
					assert.Equal(t, "receive", attrs["network.io.direction"])
					bytesByDestination[attrs["proxy.destination_country"].(string)] += dp.Value
				}
			}
		}
		assert.Equal(t, map[string]int64{"DE": 5, "IR": 5, "n/a": 5}, bytesByDestination,
			"an IP destination and a resolved domestic domain count under their country, an unresolved domain under n/a")
	})
}
