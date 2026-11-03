package metrics

import (
	"testing"

	semconv "github.com/getlantern/semconv"
	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

func testAttributes() *attributes {
	a := &attributes{
		attrs: []attribute.KeyValue{
			semconv.NetworkProtocolNameKey.String("tls"),
			semconv.ProxyInboundKey.String("ss-in"),
			semconv.ProxyInboundTypeKey.String("shadowsocks"),
			semconv.ProxyOutboundKey.String("direct"),
		},
		client: &clientcontext.ClientInfo{DeviceID: "dev-1", Platform: "android", Version: "9.0.1"},
	}
	a.country.Store(ccNa)
	return a
}

// A connection's set is built once per direction and reused until the
// country or network lookup changes what it should hold.
func TestIOSetCachedUntilLookupLands(t *testing.T) {
	a := testAttributes()

	rx1 := a.ioSet(rx)
	cached := a.ioSets[0].Load()
	rx2 := a.ioSet(rx)
	assert.Same(t, cached, a.ioSets[0].Load(), "a second read reuses the cached set")
	assert.True(t, rx1.Equals(&rx2))

	dir, _ := rx1.Value(semconv.NetworkIODirectionKey)
	assert.Equal(t, "receive", dir.AsString())
	txSet := a.ioSet(tx)
	dir, _ = txSet.Value(semconv.NetworkIODirectionKey)
	assert.Equal(t, "transmit", dir.AsString())

	country, _ := rx1.Value(semconv.GeoCountryISOCodeKey)
	assert.Equal(t, ccNa, country.AsString())
	_, hasASN := rx1.Value(semconv.ClientAsnKey)
	assert.False(t, hasASN, "no ASN before the lookup")

	a.country.Store("IR")
	a.network.Store(&clientNetwork{asn: "AS197207", isp: "MCI"})
	rx3 := a.ioSet(rx)
	assert.NotSame(t, cached, a.ioSets[0].Load(), "the lookup result rebuilds the set")
	country, _ = rx3.Value(semconv.GeoCountryISOCodeKey)
	assert.Equal(t, "IR", country.AsString())
	asn, _ := rx3.Value(semconv.ClientAsnKey)
	assert.Equal(t, "AS197207", asn.AsString())
	isp, _ := rx3.Value(semconv.ClientISPKey)
	assert.Equal(t, "MCI", isp.AsString())
	device, _ := rx3.Value(semconv.ClientDeviceIDKey)
	assert.Equal(t, "dev-1", device.AsString())
}

// The cached set holds the same attributes the per-report build did.
func TestIOSetMatchesPerReportAttributes(t *testing.T) {
	a := metadataToAttributes(adapter.InboundContext{Protocol: "tls", Inbound: "ss-in", InboundType: "shadowsocks", Outbound: "direct"})
	a.client = &clientcontext.ClientInfo{DeviceID: "dev-2", Platform: "ios", IsPro: true, Version: "9.1"}
	a.country.Store("IR")
	a.network.Store(&clientNetwork{asn: "AS44244", isp: "Irancell"})

	want := append(a.AsSlice(),
		semconv.NetworkIODirectionKey.String(string(tx)),
		semconv.ClientDeviceIDKey.String("dev-2"),
		semconv.ClientAsnKey.String("AS44244"),
		semconv.ClientISPKey.String("Irancell"),
	)
	wantSet := attribute.NewSet(want...)
	got := a.ioSet(tx)
	assert.True(t, wantSet.Equals(&got), "got %v, want %v", got.ToSlice(), wantSet.ToSlice())
}

func BenchmarkIOSet(b *testing.B) {
	a := testAttributes()
	a.country.Store("IR")
	a.network.Store(&clientNetwork{asn: "AS197207", isp: "MCI"})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = a.ioSet(rx)
		}
	})
	b.Run("rebuilt", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			a.ioSets[0].Store(nil)
			_ = a.ioSet(rx)
		}
	})
}
