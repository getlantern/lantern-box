package box

import (
	"net/netip"
	"testing"

	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
)

func TestBaseContextIncludesPhysicalEndpointRegistry(t *testing.T) {
	registry := service.FromContext[adapter.PhysicalEndpointRegistry](BaseContext())
	require.NotNil(t, registry)
	require.False(t, registry.Contains("proxy-a", netip.MustParseAddrPort("192.0.2.10:443")))
}
