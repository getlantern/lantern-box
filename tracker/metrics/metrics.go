// Package metrics provides a metrics manager that uses OpenTelemetry to track
// various metrics related to the proxy server's performance. It includes
// tracking bytes sent and received, connection duration, and the number of
// connections. The metrics are recorded using OpenTelemetry's metric API and
// can be used for monitoring and observability purposes.
package metrics

import (
	"context"
	"time"

	"github.com/getlantern/geo"
	"github.com/getlantern/http-proxy-lantern/v2/instrument/distinct"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type metricsManager struct {
	meter    metric.Meter
	proxyIO  metric.Int64Counter
	conns    metric.Int64UpDownCounter
	duration metric.Int64Histogram

	distinctClients1m  *distinct.SlidingWindowDistinctCount
	distinctClients10m *distinct.SlidingWindowDistinctCount
	distinctClients1h  *distinct.SlidingWindowDistinctCount

	countryLookup geo.CountryLookup
}

var metrics = &metricsManager{}

func SetupMetricsManager(geolite2CityURL, cityDBFile string) {
	meter := otel.GetMeterProvider().Meter("lantern-box")

	pIO, err := meter.Int64Counter("proxy.io", metric.WithUnit("bytes"))
	if err != nil {
		pIO = &noop.Int64Counter{}
	}

	// Track the number of connections.
	conns, err := meter.Int64UpDownCounter("proxy.connections", metric.WithDescription("Number of connections"))
	if err != nil {
		conns = &noop.Int64UpDownCounter{}
	}

	// Track connection duration.
	duration, err := meter.Int64Histogram("proxy.connections.duration", metric.WithDescription("Connection duration"))
	if err != nil {
		duration = &noop.Int64Histogram{}
	}

	// Track number of devices connected over different time windows, using deviceId hashes
	distinctClients1m := distinct.NewSlidingWindowDistinctCount(time.Minute, time.Second)
	distinctClients10m := distinct.NewSlidingWindowDistinctCount(10*time.Minute, 10*time.Second)
	distinctClients1h := distinct.NewSlidingWindowDistinctCount(time.Hour, time.Minute)

	_, err = meter.Int64ObservableGauge(
		"proxy.clients.active",
		metric.WithInt64Callback(func(ctx context.Context, io metric.Int64Observer) error {
			io.Observe(int64(distinctClients1m.Cardinality()), metric.WithAttributes(attribute.String("window", "1m")))
			io.Observe(int64(distinctClients10m.Cardinality()), metric.WithAttributes(attribute.String("window", "10m")))
			io.Observe(int64(distinctClients1h.Cardinality()), metric.WithAttributes(attribute.String("window", "1h")))
			return nil
		}))
	if err != nil {
		log.Warn("failed to create observable gauge for active clients: ", err)
	}

	metrics.meter = meter
	metrics.proxyIO = pIO
	metrics.duration = duration
	metrics.conns = conns

	metrics.distinctClients1m = distinctClients1m
	metrics.distinctClients10m = distinctClients10m
	metrics.distinctClients1h = distinctClients1h

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

func countDistinctClients(deviceID string) {
	metrics.distinctClients1m.Add(deviceID)
	metrics.distinctClients10m.Add(deviceID)
	metrics.distinctClients1h.Add(deviceID)
}
