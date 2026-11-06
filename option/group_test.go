package option

import (
	"testing"

	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeTimeoutsMsDecodingNeverFailsTheConfig(t *testing.T) {
	var opts MutableAutoSelectOutboundOptions
	err := json.Unmarshal([]byte(`{
		"outbounds": ["a"],
		"probe_timeouts_ms": {"hysteria2x": 3000, "vless": -1, "tuic": 4294967296, "shadowsocks": "x", "samizdat": 2.5, "trojan": null, "wireguard": 1e400}
	}`), &opts)
	require.NoError(t, err, "a bad timeout costs only that entry")
	assert.Equal(t, ProbeTimeoutsMs{"hysteria2x": 3000}, opts.ProbeTimeoutsMs)
	assert.Equal(t, []string{"a"}, opts.Outbounds)

	var notObject MutableAutoSelectOutboundOptions
	require.NoError(t, json.Unmarshal([]byte(`{"outbounds": ["a"], "probe_timeouts_ms": [1, 2]}`), &notObject))
	assert.Empty(t, notObject.ProbeTimeoutsMs)
	assert.Equal(t, []string{"a"}, notObject.Outbounds)
}
