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
	meter          metric.Meter
	ProxyIO        metric.Int64Counter
	conns          metric.Int64UpDownCounter
	duration       metric.Int64Histogram
	sessionGoodput metric.Float64Histogram

	countryLookup  geo.CountryLookup
	countryLookupC chan countryLookupRequest

	asnLookup  geo.ISPLookup
	asnLookupC chan asnLookupRequest
}

var metrics = &metricsManager{
	ProxyIO:        &noop.Int64Counter{},
	conns:          &noop.Int64UpDownCounter{},
	duration:       &noop.Int64Histogram{},
	sessionGoodput: &noop.Float64Histogram{},
	countryLookup:  geo.NoLookup{},
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
	// Track per-session download goodput (received bytes per second of connection
	// lifetime), recorded once at close for sessions that moved at least
	// goodputMinBytes. Sliceable by track (resource attr) × cloud.region (resource
	// attr) × geo.country.iso_code (point attr) so the bandit experiment evaluator
	// can compare a challenger track's median goodput against the incumbent's. Unit
	// "bytes/s" matches proxy.io's "bytes" spelling for consistency within this
	// package's metrics.
	goodput, err := meter.Float64Histogram("proxy.session.goodput",
		metric.WithUnit("bytes/s"),
		metric.WithDescription("Per-session download goodput: received bytes per second of connection lifetime"))
	if err == nil {
		metrics.sessionGoodput = goodput
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
