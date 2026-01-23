// Package metrics provides a metrics manager that uses OpenTelemetry to track
// various metrics related to the proxy server's performance. It includes
// tracking bytes sent and received, connection duration, and the number of
// connections. The metrics are recorded using OpenTelemetry's metric API and
// can be used for monitoring and observability purposes.
package metrics

import (
	"time"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type metricsManager struct {
	meter         metric.Meter
	ProxyIO       metric.Int64Counter
	Connections   metric.Int64Counter
	conns         metric.Int64UpDownCounter
	duration      metric.Int64Histogram
	countryLookup geo.CountryLookup
}

var metrics = &metricsManager{}

func SetupMetricsManager(geolite2CityURL, cityDBFile string) {
	meter := otel.GetMeterProvider().Meter("lantern-box")

	pIO, err := meter.Int64Counter("proxy.io", metric.WithUnit("bytes"))
	if err != nil {
		pIO = &noop.Int64Counter{}
	}

	connections, err := meter.Int64Counter("proxy.connections")
	if err != nil {
		connections = &noop.Int64Counter{}
	}

	// Track the number of connections.
	conns, err := meter.Int64UpDownCounter("sing.connections", metric.WithDescription("Number of connections"))
	if err != nil {
		conns = &noop.Int64UpDownCounter{}
	}

	// Track connection duration.
	duration, err := meter.Int64Histogram("sing.connection_duration", metric.WithDescription("Connection duration"))
	if err != nil {
		duration = &noop.Int64Histogram{}
	}

	metrics.meter = meter
	metrics.ProxyIO = pIO
	metrics.duration = duration
	metrics.Connections = connections
	metrics.conns = conns

	metrics.countryLookup = geo.FromWeb(geolite2CityURL, cityDBFile, 24*time.Hour, cityDBFile, geo.CountryCode)
	if metrics.countryLookup == nil {
		metrics.countryLookup = geo.NoLookup{}
	}

	log.Info("metrics manager set up completed")
}

func metadataToAttributes(metadata *adapter.InboundContext) []attribute.KeyValue {
	// Convert metadata to attributes
	fromCountry := metrics.countryLookup.CountryCode(metadata.Source.IPAddr().IP)
	return []attribute.KeyValue{
		attribute.String("country", fromCountry),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("inbound", metadata.Inbound),
		attribute.String("inbound_type", metadata.InboundType),
		attribute.String("outbound", metadata.Outbound),
	}
}
