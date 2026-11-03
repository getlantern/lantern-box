package metrics

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

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

// fakeResolver answers from hosts, counting lookups. A host it doesn't list
// fails to resolve.
type fakeResolver struct {
	hosts   map[string]string
	lookups map[string]int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if r.lookups == nil {
		r.lookups = map[string]int{}
	}
	r.lookups[host]++
	ip, ok := r.hosts[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return []netip.Addr{netip.MustParseAddr(ip)}, nil
}

func TestDestinationCountry(t *testing.T) {
	lookup := countryByIP{"1.2.3.4": "DE", "5.6.7.8": "US"}
	resolver := &fakeResolver{hosts: map[string]string{"example.com": "5.6.7.8"}}
	d := newDestinationCountries(lookup, resolver)

	assert.Equal(t, "DE", d.country(M.ParseSocksaddr("1.2.3.4:443"), nil), "an IP destination is looked up directly")
	assert.Equal(t, "DE", d.country(M.ParseSocksaddr("[::ffff:1.2.3.4]:443"), nil), "a 4-in-6 address is looked up as IPv4")
	assert.Equal(t, "US", d.country(M.ParseSocksaddr("example.org:443"), []netip.Addr{netip.MustParseAddr("5.6.7.8")}),
		"addresses routing resolved are used without another lookup")
	assert.Zero(t, resolver.lookups["example.org"])

	assert.Equal(t, "US", d.country(M.ParseSocksaddr("example.com:443"), nil), "a domain is resolved")
	assert.Equal(t, "US", d.country(M.ParseSocksaddr("example.com:80"), nil))
	assert.Equal(t, 1, resolver.lookups["example.com"], "a domain's country is cached")

	assert.Empty(t, d.country(M.ParseSocksaddr("dead.example:443"), nil), "an unresolvable domain has no country")
	assert.Empty(t, d.country(M.ParseSocksaddr("dead.example:443"), nil))
	assert.Equal(t, 1, resolver.lookups["dead.example"], "a failed resolution is cached too")

	assert.Empty(t, d.country(M.Socksaddr{}, nil), "an empty destination has no country")
}

func TestDestinationCountryCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver := &fakeResolver{hosts: map[string]string{"example.com": "1.2.3.4"}}
		d := newDestinationCountries(countryByIP{"1.2.3.4": "DE"}, resolver)

		d.country(M.ParseSocksaddr("example.com:443"), nil)
		time.Sleep(destinationCacheTTL - time.Second)
		d.country(M.ParseSocksaddr("example.com:443"), nil)
		assert.Equal(t, 1, resolver.lookups["example.com"], "still cached inside the TTL")

		time.Sleep(2 * time.Second)
		d.country(M.ParseSocksaddr("example.com:443"), nil)
		assert.Equal(t, 2, resolver.lookups["example.com"], "resolved again once the TTL passes")
	})
}

func TestProxyDestinationIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := metric.NewManualReader()
		sdkotel.SetMeterProvider(metric.NewMeterProvider(metric.WithReader(reader)))

		prevResolver := destinationResolver
		destinationResolver = &fakeResolver{hosts: map[string]string{"bank.example.ir": "5.6.7.8"}}
		SetupMetricsManager(countryByIP{"1.2.3.4": "DE"}, "")
		defer func() {
			stopLookupWorkers()
			destinationResolver = prevResolver
		}()

		ctx := context.Background()
		tracker := NewTracker(ctx)
		defer tracker.Close()

		for _, dest := range []string{"1.2.3.4:443", "bank.example.ir:443"} {
			client, server := net.Pipe()
			tracked := tracker.RoutedConnection(ctx, infoConn{Conn: server, info: testInfo},
				adapter.InboundContext{Destination: M.ParseSocksaddr(dest)}, nil, nil)
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
		assert.Equal(t, map[string]int64{"DE": 5, "IR": 5}, bytesByDestination,
			"an IP destination and a resolved domestic domain each count under their country")
	})
}
