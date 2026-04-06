// Package metrics provides a metrics manager that uses OpenTelemetry to track
// various metrics related to the proxy server's performance. It includes
// tracking bytes sent and received, connection duration, and the number of
// connections. The metrics are recorded using OpenTelemetry's metric API and
// can be used for monitoring and observability purposes.
package metrics

import (
	"time"

	"github.com/getlantern/geo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type metricsManager struct {
	meter    metric.Meter
	ProxyIO  metric.Int64Counter
	conns    metric.Int64UpDownCounter
	duration metric.Int64Histogram

	countryLookup  geo.CountryLookup
	lookupTimeout  time.Duration
}

var metrics = &metricsManager{
	ProxyIO:       &noop.Int64Counter{},
	conns:         &noop.Int64UpDownCounter{},
	duration:      &noop.Int64Histogram{},
	countryLookup: geo.NoLookup{},
}

// SetupMetricsManager initialises the global metrics manager. lookupTimeout
// controls how long an IP-based geo lookup may take before it is abandoned and
// the client-reported country is used as a fallback; pass 0 to call the lookup
// synchronously with no timeout.
func SetupMetricsManager(countryLookup geo.CountryLookup, lookupTimeout time.Duration) {
	meter := otel.GetMeterProvider().Meter("lantern-box")
	if pIO, err := meter.Int64Counter("proxy.io", metric.WithUnit("bytes")); err == nil {
		metrics.ProxyIO = pIO
	}
	// Track the number of connections.
	conns, err := meter.Int64UpDownCounter("sing.connections", metric.WithDescription("Number of connections"))
	if err == nil {
		metrics.conns = conns
	}
	// Track connection duration.
	duration, err := meter.Int64Histogram("sing.connection_duration", metric.WithDescription("Connection duration"))
	if err == nil {
		metrics.duration = duration
	}

	if countryLookup != nil {
		metrics.countryLookup = countryLookup
	}
	metrics.lookupTimeout = lookupTimeout
	metrics.meter = meter
}
