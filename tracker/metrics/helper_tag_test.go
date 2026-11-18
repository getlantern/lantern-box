package metrics

import (
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
)

func TestMetricAttributes_RedactHelperTags(t *testing.T) {
	md := adapter.InboundContext{Inbound: "helper-abcdefabcdef", Outbound: "helper-0123456789ab"}
	for name, attrs := range map[string][]attribute.KeyValue{
		"metadataToAttributes": metadataToAttributes(md).attrs,
		"unidentifiedAttrs":    unidentifiedAttrs(md),
	} {
		for _, kv := range attrs {
			assert.NotContains(t, kv.Value.Emit(), "0123456789ab", "%s %s", name, kv.Key)
			assert.NotContains(t, kv.Value.Emit(), "abcdefabcdef", "%s %s", name, kv.Key)
		}
	}

	pool := metadataToAttributes(adapter.InboundContext{Inbound: "in", Outbound: "ss-out-pool1"}).attrs
	assert.Contains(t, pool, attribute.String("proxy.outbound", "ss-out-pool1"), "pool tags are unchanged")
}
