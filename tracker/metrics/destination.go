package metrics

import (
	"net"
	"net/netip"
	"sync/atomic"

	"github.com/getlantern/geo"
	semconv "github.com/getlantern/semconv"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"go.opentelemetry.io/otel/attribute"
)

const destinationLookupWorkers = 4

type destinationLookupRequest struct {
	destination M.Socksaddr
	// addresses are the destination's addresses when routing already resolved
	// them (metadata.DestinationAddresses).
	addresses []netip.Addr
	country   *atomic.Value
}

// destinationCountry returns the country of the address the outbound dials, or
// "" when that isn't known yet. A domain destination is never resolved here:
// an independent lookup would send the domain to the system resolver, which
// is not the resolver sing-box dials through when the config sets one (a
// residential peer resolves over DoH), and could pick a different address.
// Its country is known only when a route "resolve" action ran before routing.
func destinationCountry(lookup geo.CountryLookup, dest M.Socksaddr, addrs []netip.Addr) string {
	switch {
	case dest.IsIP():
		return countryOf(lookup, dest.Addr)
	case len(addrs) > 0:
		return countryOf(lookup, addrs[0])
	}
	return ""
}

func countryOf(lookup geo.CountryLookup, addr netip.Addr) string {
	return lookup.CountryCode(net.IP(addr.Unmap().AsSlice()))
}

func destinationLookupWorker(ch <-chan destinationLookupRequest, lookup geo.CountryLookup) {
	for req := range ch {
		if c := destinationCountry(lookup, req.destination, req.addresses); c != "" {
			req.country.Store(c)
		}
	}
}

// requestDestinationCountry queues the lookup of the destination's country
// into attrs, dropping it when the workers are saturated, as the client's
// country lookup does. A dropped lookup leaves the country n/a.
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
