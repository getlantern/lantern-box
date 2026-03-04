package metrics

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
			attrs := append(r.attrs.AsSlice(), attribute.KeyValue{
				Key:   "direction",
				Value: attribute.StringValue(string(r.direction)),
			})
			metrics.ProxyIO.Add(context.Background(), int64(r.n), metric.WithAttributes(attrs...))
		}
	}
}

func (t *MetricsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	attrs := metadataToAttributes(metadata)
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewConn(conn, attrs, t)
}

func (t *MetricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	attrs := metadataToAttributes(metadata)
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return NewPacketConn(conn, attrs, t)
}

func (t *MetricsTracker) Leave(duration int64, attrs *attributes) {
	a := append(attrs.attrs, attribute.KeyValue{
		Key:   "country",
		Value: attribute.StringValue(attrs.country.Load().(string)),
	})
	metrics.duration.Record(context.Background(), duration, metric.WithAttributes(a...))
	metrics.conns.Add(context.Background(), -1, metric.WithAttributes(a...))
}

type attributes struct {
	attrs   []attribute.KeyValue
	country atomic.Value // string
}

func (a *attributes) AsSlice() []attribute.KeyValue {
	return append(a.attrs, attribute.KeyValue{
		Key:   "country",
		Value: attribute.StringValue(a.country.Load().(string)),
	})
}

func metadataToAttributes(metadata adapter.InboundContext) *attributes {
	attrs := &attributes{
		attrs: []attribute.KeyValue{
			attribute.String("protocol", metadata.Protocol),
			attribute.String("inbound", metadata.Inbound),
			attribute.String("inbound_type", metadata.InboundType),
			attribute.String("outbound", metadata.Outbound),
		},
	}
	attrs.country.Store(ccNa)
	go func() {
		fromCountry := metrics.countryLookup.CountryCode(metadata.Source.IPAddr().IP)
		attrs.country.Store(fromCountry)
	}()
	return attrs
}
