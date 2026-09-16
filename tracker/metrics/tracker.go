package metrics

import (
	"context"
	"net"
	"sync/atomic"

	semconv "github.com/getlantern/semconv"
	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

const (
	rx ioAttr = "receive"
	tx ioAttr = "transmit"

	ccNa = "n/a"
)

type ioAttr string

var _ (adapter.ConnectionTracker) = (*MetricsTracker)(nil)

type MetricsTracker struct {
	ctx     context.Context
	cancel  context.CancelFunc
	reportC chan report
}

func NewTracker(ctx context.Context) *MetricsTracker {
	ctx, cancel := context.WithCancel(ctx)
	t := &MetricsTracker{
		ctx:     ctx,
		cancel:  cancel,
		reportC: make(chan report, 100),
	}
	go trackIOLoop(ctx, t.reportC)
	return t
}

func (t *MetricsTracker) Close() error {
	t.cancel()
	return nil
}

type report struct {
	n         int
	direction ioAttr
	attrs     *attributes
}

func (t *MetricsTracker) TrackIO(d ioAttr, n int, attrs *attributes) {
	select {
	case <-t.ctx.Done():
	case t.reportC <- report{n, d, attrs}:
	}
}

func trackIOLoop(ctx context.Context, reportC <-chan report) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-reportC:
			attrs := append(r.attrs.AsSlice(),
				semconv.NetworkIODirectionKey.String(string(r.direction)),
			)
			if r.attrs.client != nil {
				attrs = append(attrs,
					semconv.ClientDeviceIDKey.String(r.attrs.client.DeviceID),
				)
			}
			metrics.ProxyIO.Add(context.Background(), int64(r.n), metric.WithAttributes(attrs...))
		}
	}
}

// emitDeviceConnectedSpan emits a correlation span for a
// device_id's connection to the proxy, to be correlated with the
// client's API proxy assignment to assess connectivity success rate
// and time-to-connect differences across connections.
func emitDeviceConnectedSpan(ctx context.Context, info clientcontext.ClientInfo) {
	tracer := otel.Tracer("lantern-box")
	_, span := tracer.Start(ctx, "device_id.connected")
	span.SetAttributes(
		semconv.ClientDeviceIDKey.String(info.DeviceID),
		semconv.ClientPlatformKey.String(info.Platform),
		semconv.ClientIsProKey.Bool(info.IsPro),
		semconv.GeoCountryISOCodeKey.String(info.CountryCode),
		semconv.ClientVersionKey.String(info.Version),
	)
	span.End()
}

func (t *MetricsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	attrs := metadataToAttributes(metadata)
	if info, ok := clientcontext.InfoFromConn(conn); ok {
		attrs.client = &info
		emitDeviceConnectedSpan(ctx, info)
	}
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewConn(conn, attrs, t)
}

func (t *MetricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	attrs := metadataToAttributes(metadata)
	if info, ok := clientcontext.InfoFromConn(conn); ok {
		attrs.client = &info
		emitDeviceConnectedSpan(ctx, info)
	}
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewPacketConn(conn, attrs, t)
}

func (t *MetricsTracker) Leave(duration int64, attrs *attributes) {
	a := attrs.AsSlice()
	metrics.duration.Record(context.Background(), duration, metric.WithAttributes(a...))
	metrics.conns.Add(context.Background(), -1, metric.WithAttributes(a...))
}

// recordGoodput records a session's download goodput (received bytes per
// second of connection lifetime) at close. It emits for every session that
// moved any bytes over a non-zero duration — there is NO byte floor.
//
// The floor used to be 1 MB, but the floored direction is the small
// client→proxy (upload / "receive") side, which averages ~22 KB/session in
// prod — ~45× under the old 1 MB floor. Only ~0.04% of sessions ever cleared
// it, so the floor erased the goodput signal for the ~99.96% of real (probe,
// connectivity-check, blocked-then-retry, small-page) traffic that dominates
// censored markets, and the bandit evaluator false-retired live challengers as
// "starved". Very short/tiny sessions do produce noisy per-second rates, but
// the evaluator compares per-(track, country) p50 medians, which are robust to
// that tail — so we keep the sample rather than drop, cap, or weight it.
//
// durationMs is the connection's open time; it includes idle periods, so this
// is a floor on true transfer speed — but both arms of a bandit experiment are
// measured identically, so it's a fair relative signal.
//
// The sample carries the track (point-attr key "track", not the resource attr
// "proxy.track") and network.io.direction='receive' as point (not resource)
// attributes, plus geo.country.iso_code via attrs.AsSlice(), so the evaluator
// can filter/group by track and country.
func (t *MetricsTracker) recordGoodput(rxBytes, durationMs int64, attrs *attributes) {
	if rxBytes <= 0 || durationMs <= 0 {
		return
	}
	goodput := float64(rxBytes) / (float64(durationMs) / 1000.0)
	// Copy into a slice sized for the extra elements rather than appending onto
	// AsSlice()'s result in place, so we never share a backing array with a
	// concurrent reporter.
	base := attrs.AsSlice()
	a := make([]attribute.KeyValue, 0, len(base)+2)
	a = append(a, base...)
	a = append(a,
		semconv.NetworkIODirectionKey.String(string(rx)),
		// The evaluator filters `track IN [...]` and groups by `track` on the
		// bare, literal point-attribute key "track" (lantern-cloud
		// GoodputByStratum: filter + queryScalar groupKeys{"track", ...} +
		// series label s.labels["track"]). track is ALSO an OTEL resource attr,
		// but keyed "proxy.track" (semconv.ProxyTrackKey) and the metrics
		// pipeline doesn't expose resource attrs as queryable labels — so the
		// point attr must use the bare "track" key, NOT semconv.ProxyTrackKey.
		// (Mirrors http-proxy #675's attribute.String("track", ...).)
		attribute.String("track", metrics.track),
	)
	metrics.sessionGoodput.Record(context.Background(), goodput, metric.WithAttributes(a...))
}

type attributes struct {
	attrs   []attribute.KeyValue
	country atomic.Value // string
	client  *clientcontext.ClientInfo
}

func (a *attributes) AsSlice() []attribute.KeyValue {
	s := append(a.attrs,
		semconv.GeoCountryISOCodeKey.String(a.country.Load().(string)),
	)
	if a.client != nil {
		s = append(s,
			semconv.ClientPlatformKey.String(a.client.Platform),
			semconv.ClientIsProKey.Bool(a.client.IsPro),
			semconv.ClientVersionKey.String(a.client.Version),
		)
	}
	return s
}

func metadataToAttributes(metadata adapter.InboundContext) *attributes {
	attrs := &attributes{
		attrs: []attribute.KeyValue{
			semconv.NetworkProtocolNameKey.String(metadata.Protocol),
			semconv.ProxyInboundKey.String(metadata.Inbound),
			semconv.ProxyInboundTypeKey.String(metadata.InboundType),
			semconv.ProxyOutboundKey.String(metadata.Outbound),
		},
	}
	attrs.country.Store(ccNa)
	if metrics.countryLookupC != nil {
		select {
		case metrics.countryLookupC <- countryLookupRequest{
			ip:      metadata.Source.IPAddr().IP,
			country: &attrs.country,
		}:
		default:
		}
	}
	return attrs
}
