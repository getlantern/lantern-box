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

// goodputBucketBoundaries are the explicit bucket boundaries, in bytes/s, for
// the proxy.session.goodput histogram: a half-decade log scale from 1 B/s to
// 10 MB/s. Session goodput spans ~6 decades — idle keepalive sessions sit
// below 100 B/s while real page loads run 100 kB/s–10 MB/s — and the SDK
// default boundaries top out at 10,000 B/s, which censored everything faster
// than 10 kB/s into the final bucket (IR's daily p90 read as exactly 10000).
// A log scale gives every decade the same relative resolution, so p50/p90/p99
// interpolate to meaningful values across the whole range.
//
// Exactly 15 boundaries, matching the SDK default's count: SigNoz stores one
// sample per bucket per series per export interval, so keeping the count
// identical keeps the metric's ingest cost identical (this family is ~20% of
// metric ingest; see lantern-cloud #3069).
//
// http-proxy emits the same metric from instrument/otelinstrument and MUST
// use identical boundaries — SigNoz merges the two streams, and quantiles
// over mixed bucket layouts are garbage.
var goodputBucketBoundaries = []float64{
	1, 3, 10, 30, 100, 300,
	1_000, 3_000, 10_000, 30_000, 100_000, 300_000,
	1_000_000, 3_000_000, 10_000_000,
}

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
		metric.WithDescription("Per-session download goodput: received bytes per second of connection lifetime"),
		metric.WithExplicitBucketBoundaries(goodputBucketBoundaries...))
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
