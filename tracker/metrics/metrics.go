// Package metrics provides a metrics manager that uses OpenTelemetry to track
// various metrics related to the proxy server's performance. It includes
// tracking bytes sent and received, connection duration, and the number of
// connections. The metrics are recorded using OpenTelemetry's metric API and
// can be used for monitoring and observability purposes.
package metrics

import (
	"net"
	"sync/atomic"

	"github.com/getlantern/geo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	countryLookupWorkers = 4
	asnLookupWorkers     = 4
)

type countryLookupRequest struct {
	ip      net.IP
	country *atomic.Value
}

type asnLookupRequest struct {
	ip  net.IP
	asn *atomic.Value
}

type metricsManager struct {
	meter    metric.Meter
	ProxyIO  metric.Int64Counter
	conns    metric.Int64UpDownCounter
	duration metric.Int64Histogram

	countryLookup  geo.CountryLookup
	countryLookupC chan countryLookupRequest

	asnLookup  geo.ISPLookup
	asnLookupC chan asnLookupRequest
}

var metrics = &metricsManager{
	ProxyIO:       &noop.Int64Counter{},
	conns:         &noop.Int64UpDownCounter{},
	duration:      &noop.Int64Histogram{},
	countryLookup: geo.NoLookup{},
}

func SetupMetricsManager(countryLookup geo.CountryLookup, asnLookup geo.ISPLookup) {
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
	if _, ok := countryLookup.(geo.NoLookup); !ok {
		metrics.countryLookupC = make(chan countryLookupRequest, 256)
		for range countryLookupWorkers {
			go countryLookupWorker(metrics.countryLookupC, metrics.countryLookup)
		}
	}

	if asnLookup != nil {
		metrics.asnLookup = asnLookup
	}
	if _, ok := asnLookup.(geo.NoLookup); !ok && asnLookup != nil {
		metrics.asnLookupC = make(chan asnLookupRequest, 256)
		for range asnLookupWorkers {
			go asnLookupWorker(metrics.asnLookupC, metrics.asnLookup)
		}
	}

	metrics.meter = meter
}

func countryLookupWorker(ch <-chan countryLookupRequest, lookup geo.CountryLookup) {
	for req := range ch {
		req.country.Store(lookup.CountryCode(req.ip))
	}
}

func asnLookupWorker(ch <-chan asnLookupRequest, lookup geo.ISPLookup) {
	for req := range ch {
		req.asn.Store(lookup.ASN(req.ip))
	}
}
