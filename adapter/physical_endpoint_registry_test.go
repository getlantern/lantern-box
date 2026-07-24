package adapter

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhysicalEndpointRegistry_SetReplacesAndNormalizes(t *testing.T) {
	registry := NewPhysicalEndpointRegistry()
	ipv4 := netip.MustParseAddrPort("192.0.2.10:443")
	mapped := netip.MustParseAddrPort("[::ffff:192.0.2.10]:443")
	ipv6 := netip.MustParseAddrPort("[2001:db8::10]:8443")

	registry.Set("proxy-a", []netip.AddrPort{mapped, mapped, {}, netip.AddrPortFrom(ipv4.Addr(), 0)})

	assert.True(t, registry.Contains("proxy-a", ipv4), "IPv4-mapped addresses should match canonical IPv4")
	assert.False(t, registry.Contains("proxy-a", netip.AddrPortFrom(ipv4.Addr(), 444)), "ports must match exactly")
	assert.False(t, registry.Contains("proxy-a", ipv6))

	registry.Set("proxy-a", []netip.AddrPort{ipv6})

	assert.False(t, registry.Contains("proxy-a", ipv4), "Set should replace the prior endpoint set")
	assert.True(t, registry.Contains("proxy-a", ipv6))

	registry.Delete("proxy-a")
	assert.False(t, registry.Contains("proxy-a", ipv6))
}

func TestPhysicalEndpointRegistry_EmptySetDeletesTag(t *testing.T) {
	registry := NewPhysicalEndpointRegistry()
	endpoint := netip.MustParseAddrPort("192.0.2.10:443")
	registry.Set("proxy-a", []netip.AddrPort{endpoint})

	registry.Set("proxy-a", nil)

	assert.False(t, registry.Contains("proxy-a", endpoint))
}

func TestProxyRoutingLoopError(t *testing.T) {
	err := &ProxyRoutingLoopError{OutboundTag: "proxy-a"}

	require.ErrorIs(t, err, ErrProxyRoutingLoop)
	assert.Equal(t, "proxy-a", err.OutboundTag)
	assert.NotContains(t, err.Error(), "192.0.2.10")

	var typed *ProxyRoutingLoopError
	require.True(t, errors.As(err, &typed))
	assert.Same(t, err, typed)
}
