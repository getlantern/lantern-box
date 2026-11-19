package metrics

import (
	"math/rand/v2"
	"net/netip"
	"os"
	"testing"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// benchSources returns n client addresses spread over the IPv4 space, so the
// lookups walk different paths through the database. It keeps only addresses
// with a country: the geo package logs every miss, which would dominate.
func benchSources(n int, lookup geo.CountryLookup) []adapter.InboundContext {
	r := rand.New(rand.NewPCG(1, 2))
	mds := make([]adapter.InboundContext, 0, n)
	for len(mds) < n {
		var a [4]byte
		for j := range a {
			a[j] = byte(r.IntN(256))
		}
		addr := netip.AddrFrom4(a)
		if !addr.IsGlobalUnicast() || addr.IsPrivate() {
			continue
		}
		if lookup.CountryCode(addr.AsSlice()) == "" {
			continue
		}
		var md adapter.InboundContext
		md.Source = M.SocksaddrFrom(addr, 443)
		mds = append(mds, md)
	}
	return mds
}

// BenchmarkMetadataToAttributes measures building a connection's attributes,
// which runs on the connection path. It needs the production databases:
//
//	GEO_COUNTRY_DB=GeoLite2-Country.mmdb GEO_ISP_DB=GeoIP2-ISP.mmdb \
//	  go test -run '^$' -bench MetadataToAttributes ./tracker/metrics/
func BenchmarkMetadataToAttributes(b *testing.B) {
	countryPath, ispPath := os.Getenv("GEO_COUNTRY_DB"), os.Getenv("GEO_ISP_DB")
	if countryPath == "" || ispPath == "" {
		b.Skip("GEO_COUNTRY_DB and GEO_ISP_DB not set")
	}
	country, err := geo.FromFile(countryPath)
	if err != nil {
		b.Fatalf("loading %s: %v", countryPath, err)
	}
	isp, err := geo.FromFile(ispPath)
	if err != nil {
		b.Fatalf("loading %s: %v", ispPath, err)
	}
	SetISPLookup(isp)
	SetupMetricsManager(country, "")
	b.Cleanup(stopLookupWorkers)

	mds := benchSources(4096, country)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		metadataToAttributes(mds[i%len(mds)])
		i++
	}
}
