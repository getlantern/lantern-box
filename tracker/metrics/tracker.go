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
	otelsc "go.opentelemetry.io/otel/semconv/v1.37.0"

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
				otelsc.NetworkIODirectionKey.String(string(r.direction)),
			)
			metrics.ProxyIO.Add(context.Background(), int64(r.n), metric.WithAttributes(attrs...))
		}
	}
}

// emitDeviceConnectedSpan emits a correlation span for a
// device_id's connection to the proxy, to be correlated with the
// client's API proxy assignment to assess connectivity success rate
// and time-to-connect differences across connections.
func emitDeviceConnectedSpan(ctx context.Context) {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if !ok {
		return
	}
	tracer := otel.Tracer("lantern-box")
	_, span := tracer.Start(ctx, "device_id.connected")
	span.SetAttributes(
		semconv.ClientDeviceIDKey.String(info.DeviceID),
		semconv.ClientPlatformKey.String(info.Platform),
		semconv.ClientIsProKey.Bool(info.IsPro),
		otelsc.GeoCountryISOCodeKey.String(info.CountryCode),
		semconv.ClientVersionKey.String(info.Version),
	)
	span.End()
}

func (t *MetricsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	emitDeviceConnectedSpan(ctx)
	attrs := metadataToAttributes(metadata)
	if info, ok := clientcontext.ClientInfoFromContext(ctx); ok {
		attrs.client = &info
	}
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewConn(conn, attrs, t)
}

func (t *MetricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	emitDeviceConnectedSpan(ctx)
	attrs := metadataToAttributes(metadata)
	if info, ok := clientcontext.ClientInfoFromContext(ctx); ok {
		attrs.client = &info
	}
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewPacketConn(conn, attrs, t)
}

func (t *MetricsTracker) Leave(duration int64, attrs *attributes) {
	a := append(attrs.attrs,
		otelsc.GeoCountryISOCodeKey.String(attrs.country.Load().(string)),
	)
	if attrs.client != nil {
		a = append(a,
			semconv.ClientDeviceIDKey.String(attrs.client.DeviceID),
			semconv.ClientPlatformKey.String(attrs.client.Platform),
			semconv.ClientIsProKey.Bool(attrs.client.IsPro),
			semconv.ClientVersionKey.String(attrs.client.Version),
		)
	}
	metrics.duration.Record(context.Background(), duration, metric.WithAttributes(a...))
	metrics.conns.Add(context.Background(), -1, metric.WithAttributes(a...))
}

type attributes struct {
	attrs   []attribute.KeyValue
	country atomic.Value // string
	client  *clientcontext.ClientInfo
}

func (a *attributes) AsSlice() []attribute.KeyValue {
	s := append(a.attrs,
		otelsc.GeoCountryISOCodeKey.String(a.country.Load().(string)),
	)
	if a.client != nil {
		s = append(s,
			semconv.ClientDeviceIDKey.String(a.client.DeviceID),
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
			otelsc.NetworkProtocolNameKey.String(metadata.Protocol),
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
