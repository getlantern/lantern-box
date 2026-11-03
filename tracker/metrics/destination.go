package metrics

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/getlantern/geo"
	semconv "github.com/getlantern/semconv"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"go.opentelemetry.io/otel/attribute"
)

const (
	destinationLookupWorkers = 4
	// destinationResolveTimeout bounds the lookup of a domain destination. The
	// lookup runs off the connection's path, so a slow resolver only delays the
	// destination country, which reads n/a until it lands.
	destinationResolveTimeout = 2 * time.Second
	// destinationCacheSize and destinationCacheTTL bound the domain → country
	// cache, so a busy proxy resolves each popular domain about once per TTL
	// rather than once per connection.
	destinationCacheSize = 10000
	destinationCacheTTL  = 10 * time.Minute
)

// hostResolver resolves a domain destination to its addresses.
type hostResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// destinationResolver is the resolver for domain destinations; tests replace it.
var destinationResolver hostResolver = net.DefaultResolver

type destinationLookupRequest struct {
	destination M.Socksaddr
	// addresses are the destination's addresses when routing already resolved
	// them (metadata.DestinationAddresses).
	addresses []netip.Addr
	country   *atomic.Value
}

// cachedCountry is a domain's destination country and when it stops being
// trusted.
type cachedCountry struct {
	country string
	expires time.Time
}

// destinationCountries looks up the country a connection's destination is in.
type destinationCountries struct {
	lookup   geo.CountryLookup
	resolver hostResolver
	cache    *lru.Cache[string, cachedCountry]
}

func newDestinationCountries(lookup geo.CountryLookup, resolver hostResolver) *destinationCountries {
	cache, _ := lru.New[string, cachedCountry](destinationCacheSize)
	return &destinationCountries{lookup: lookup, resolver: resolver, cache: cache}
}

// country returns the country of the destination, or "" when it can't be told.
// An IP destination, or one routing resolved, is looked up directly. A domain
// is resolved first, the way the outbound will dial it, and its country is
// cached by domain; a failed resolution is cached too, so a dead domain isn't
// retried on every connection.
func (d *destinationCountries) country(dest M.Socksaddr, addrs []netip.Addr) string {
	switch {
	case dest.IsIP():
		return d.countryOf(dest.Addr)
	case len(addrs) > 0:
		return d.countryOf(addrs[0])
	case !dest.IsFqdn():
		return ""
	}
	if c, ok := d.cache.Get(dest.Fqdn); ok && time.Now().Before(c.expires) {
		return c.country
	}
	ctx, cancel := context.WithTimeout(context.Background(), destinationResolveTimeout)
	defer cancel()
	var country string
	if resolved, err := d.resolver.LookupNetIP(ctx, "ip", dest.Fqdn); err == nil && len(resolved) > 0 {
		country = d.countryOf(resolved[0])
	}
	d.cache.Add(dest.Fqdn, cachedCountry{country: country, expires: time.Now().Add(destinationCacheTTL)})
	return country
}

func (d *destinationCountries) countryOf(addr netip.Addr) string {
	return d.lookup.CountryCode(net.IP(addr.Unmap().AsSlice()))
}

func destinationLookupWorker(ch <-chan destinationLookupRequest, countries *destinationCountries) {
	for req := range ch {
		if c := countries.country(req.destination, req.addresses); c != "" {
			req.country.Store(c)
		}
	}
}

// requestDestinationCountry queues the lookup of the destination's country
// into attrs, dropping it when the workers are saturated so that a burst of
// connections never waits on DNS. A dropped lookup leaves the country n/a.
func requestDestinationCountry(attrs *attributes, metadata adapter.InboundContext) {
	attrs.destinationCountry.Store(ccNa)
	if metrics.destinationLookupC == nil {
		return
	}
	select {
	case metrics.destinationLookupC <- destinationLookupRequest{
		destination: metadata.Destination,
		addresses:   metadata.DestinationAddresses,
		country:     &attrs.destinationCountry,
	}:
	default:
	}
}

// destinationAttrSet is a cached proxy.destination.io attribute set and the
// lookup results it was built from, rebuilt once a lookup lands.
type destinationAttrSet struct {
	country     string
	destination string
	set         attribute.Set
}

// destinationIOSet returns the proxy.destination.io attribute set for
// direction d: the client's country, the destination's country and the
// direction, and nothing that identifies a client, so the metric has at most
// one series per country pair and direction.
func (a *attributes) destinationIOSet(d ioAttr) attribute.Set {
	i := 0
	if d == tx {
		i = 1
	}
	country, _ := a.country.Load().(string)
	destination, _ := a.destinationCountry.Load().(string)
	if c := a.destinationSets[i].Load(); c != nil && c.country == country && c.destination == destination {
		return c.set
	}
	set := attribute.NewSet(
		semconv.GeoCountryISOCodeKey.String(country),
		semconv.ProxyDestinationCountryKey.String(destination),
		semconv.NetworkIODirectionKey.String(string(d)),
	)
	a.destinationSets[i].Store(&destinationAttrSet{country: country, destination: destination, set: set})
	return set
}
