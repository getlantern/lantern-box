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

const countryLookupWorkers = 4

type countryLookupRequest struct {
	ip      net.IP
	country *atomic.Value
}

type metricsManager struct {
	meter          metric.Meter
	ProxyIO        metric.Int64Counter
	conns          metric.Int64UpDownCounter
	duration       metric.Int64Histogram
	sessionGoodput metric.Float64Histogram

	// track is the proxy's experiment track (from proxy-info). It is also an
	// OTEL resource attribute (keyed "proxy.track"), but the metrics pipeline
	// does not expose resource attrs as queryable labels, so it's re-emitted as
	// a point attribute keyed "track" on the goodput sample, which is the key
	// the bandit evaluator filters/groups by.
	track string

	countryLookup  geo.CountryLookup
	countryLookupC chan countryLookupRequest
}

var metrics = &metricsManager{
	ProxyIO:        &noop.Int64Counter{},
	conns:          &noop.Int64UpDownCounter{},
	duration:       &noop.Int64Histogram{},
	sessionGoodput: &noop.Float64Histogram{},
	countryLookup:  geo.NoLookup{},
}

func SetupMetricsManager(countryLookup geo.CountryLookup, track string) {
	metrics.track = track
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
	// lifetime), recorded once at close for any session that moved >0 bytes over a
	// >0 duration (see recordGoodput; no byte floor). The bandit experiment
	// evaluator slices this by three QUERYABLE POINT attributes — "track"
	// (the bare key, not the "proxy.track" resource attr), geo.country.iso_code,
	// and network.io.direction='receive' — to compare a challenger track's
	// median goodput against the incumbent's per (track, country). These must be
	// point attributes, not OTEL resource attributes: the metrics pipeline does
	// not expose resource attrs as queryable labels (this is why track is
	// re-emitted as a "track" point attr in recordGoodput even though it is also
	// a "proxy.track" resource attr). Unit "bytes/s" matches proxy.io's "bytes"
	// spelling for consistency within this package's metrics.
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

	metrics.meter = meter
}

func countryLookupWorker(ch <-chan countryLookupRequest, lookup geo.CountryLookup) {
	for req := range ch {
		req.country.Store(lookup.CountryCode(req.ip))
	}
}
