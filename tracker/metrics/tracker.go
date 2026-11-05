package metrics

import (
	"context"
	"hash/maphash"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	semconv "github.com/getlantern/semconv"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/sagernet/sing-box/adapter"
	tun "github.com/sagernet/sing-tun"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
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
			metrics.ProxyIO.Add(context.Background(), int64(r.n), metric.WithAttributeSet(r.attrs.ioSet(r.direction)))
			metrics.destinationIO.Add(context.Background(), int64(r.n), metric.WithAttributeSet(r.attrs.destinationIOSet(r.direction)))
		}
	}
}

const (
	// deviceSpanTTL is how long a device's first connection suppresses the
	// device_id.connected span for its later connections.
	deviceSpanTTL = time.Hour
	// deviceSpanMaxDevices bounds the devices remembered; past it the least
	// recently seen are forgotten, and their next connection emits again.
	deviceSpanMaxDevices = 65536
)

// deviceSpans lets the device_id.connected span through once per device per
// deviceSpanTTL. The span marks a device's initial connection, which the API
// correlates with its proxy assignment; emitting it on every connection sent
// the collector one span per connection (~1,600/s on a busy phost), almost all
// of which were discarded downstream.
var deviceSpans = newDeviceSpanGate(deviceSpanMaxDevices, deviceSpanTTL)

// deviceSpanGate keys devices by a hash of their ID, because the ID comes from
// the client: storing it as is would let long IDs grow each entry without
// bound. A collision only suppresses one span.
type deviceSpanGate struct {
	mu   sync.Mutex
	seed maphash.Seed
	seen *expirable.LRU[uint64, struct{}]
}

func newDeviceSpanGate(size int, ttl time.Duration) *deviceSpanGate {
	return &deviceSpanGate{seed: maphash.MakeSeed(), seen: expirable.NewLRU[uint64, struct{}](size, nil, ttl)}
}

// first reports whether deviceID has not been seen within the TTL, and records
// it, so concurrent first connections from one device emit a single span. Get,
// unlike Contains, treats an expired entry as absent before the background
// cleanup removes it, and marks a returning device recently used (without
// extending its TTL) so it is not the next one evicted.
func (g *deviceSpanGate) first(deviceID string) bool {
	key := maphash.String(g.seed, deviceID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.seen.Get(key); seen {
		return false
	}
	g.seen.Add(key, struct{}{})
	return true
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
	attrs, ok := connected(ctx, conn, metadata)
	if !ok {
		return newUnidentifiedConn(conn, metadata)
	}
	return NewConn(conn, attrs, t)
}

func (t *MetricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	attrs, ok := connected(ctx, conn, metadata)
	if !ok {
		return newUnidentifiedPacketConn(conn, metadata)
	}
	return NewPacketConn(conn, attrs, t)
}

// connected records a new connection from an identified client and returns its
// attributes. It reports false, recording nothing, if conn has no client info.
func connected(ctx context.Context, conn any, metadata adapter.InboundContext) (*attributes, bool) {
	info, ok := clientcontext.InfoFromConn(conn)
	if !ok {
		return nil, false
	}
	attrs := metadataToAttributes(metadata)
	attrs.client = &info
	if deviceSpans.first(info.DeviceID) {
		emitDeviceConnectedSpan(ctx, info)
	}
	metrics.conns.Add(context.Background(), 1, metric.WithAttributes(attrs.AsSlice()...))
	return attrs, true
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
	country atomic.Value                  // string
	network atomic.Pointer[clientNetwork] // proxy.io only; nil until looked up
	client  *clientcontext.ClientInfo
	// ioSets caches the proxy.io attribute set for each direction (rx, tx).
	// Building a set sorts and deduplicates its attributes, which is too
	// costly to repeat on every read and write.
	ioSets [2]atomic.Pointer[ioAttrSet]
	// destinationCountry is the destination's country (string), n/a until
	// looked up; destinationSets caches the proxy.destination.io sets.
	destinationCountry atomic.Value
	destinationSets    [2]atomic.Pointer[destinationAttrSet]
	// destination and destinationAddresses are the connection's destination as
	// routed, and destinationMixed is set once a packet connection sends to
	// another one (notePacketDestination).
	destination          M.Socksaddr
	destinationAddresses []netip.Addr
	destinationMixed     atomic.Bool
}

// ioAttrSet is a cached proxy.io attribute set and the lookup results it was
// built from. The country and network are filled in asynchronously after the
// connection starts, so a set built before they land is rebuilt once they do.
type ioAttrSet struct {
	country string
	network *clientNetwork
	set     attribute.Set
}

// ioSet returns the proxy.io attribute set for direction d.
func (a *attributes) ioSet(d ioAttr) attribute.Set {
	i := 0
	if d == tx {
		i = 1
	}
	country, _ := a.country.Load().(string)
	network := a.network.Load()
	if c := a.ioSets[i].Load(); c != nil && c.country == country && c.network == network {
		return c.set
	}
	attrs := append(a.AsSlice(), semconv.NetworkIODirectionKey.String(string(d)))
	if a.client != nil {
		attrs = append(attrs, semconv.ClientDeviceIDKey.String(a.client.DeviceID))
	}
	// Left off when unknown, so a proxy without the ISP database
	// emits exactly the series it did before.
	if network != nil {
		if network.asn != "" {
			attrs = append(attrs, semconv.ClientAsnKey.String(network.asn))
		}
		if network.isp != "" {
			attrs = append(attrs, semconv.ClientISPKey.String(network.isp))
		}
	}
	set := attribute.NewSet(attrs...)
	a.ioSets[i].Store(&ioAttrSet{country: country, network: network, set: set})
	return set
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
			semconv.ProxyInboundKey.String(lbAdapter.RedactHelperTags(metadata.Inbound)),
			semconv.ProxyInboundTypeKey.String(metadata.InboundType),
			semconv.ProxyOutboundKey.String(lbAdapter.RedactHelperTags(metadata.Outbound)),
		},
	}
	attrs.country.Store(ccNa)
	if metrics.countryLookupC != nil {
		select {
		case metrics.countryLookupC <- countryLookupRequest{
			ip:      metadata.Source.IPAddr().IP,
			country: &attrs.country,
			network: &attrs.network,
		}:
		default:
		}
	}
	requestDestinationCountry(attrs, metadata)
	return attrs
}

// RoutedFlow returns nil: L3-forwarded TUN flows carry no stream to wrap
// and never reach a proxy server.
func (t *MetricsTracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}
