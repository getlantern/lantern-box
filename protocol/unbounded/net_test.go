package unbounded

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/pion/transport/v5"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMonitor reports names as the VPN's own interfaces. Every other method panics, unused.
type fakeMonitor struct {
	tun.DefaultInterfaceMonitor
	names []string
}

func (m *fakeMonitor) MyInterfaces() []string { return m.names }

// fakeNetworkManager gives rtcNet an interface monitor and nothing else.
type fakeNetworkManager struct {
	adapter.NetworkManager
	monitor tun.DefaultInterfaceMonitor
}

func (m *fakeNetworkManager) InterfaceMonitor() tun.DefaultInterfaceMonitor { return m.monitor }

// hostInterface is a real interface of this machine with an address, which the test pretends is
// the tunnel.
func hostInterface(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	require.NoError(t, err)
	for _, i := range ifs {
		if addrs, _ := i.Addrs(); len(addrs) > 0 {
			return i.Name
		}
	}
	t.Skip("no interface with an address")
	return ""
}

func names(ifs []*transport.Interface) []string {
	var out []string
	for _, i := range ifs {
		out = append(out, i.Name)
	}
	return out
}

func TestRTCNetHidesTheVPNsInterfaces(t *testing.T) {
	mine := hostInterface(t)
	ctx := service.ContextWith[adapter.NetworkManager](context.Background(),
		&fakeNetworkManager{monitor: &fakeMonitor{names: []string{mine}}})
	n, err := newRTCNet(ctx, nil, nil)
	require.NoError(t, err)

	ifs, err := n.Interfaces()
	require.NoError(t, err)
	assert.NotContains(t, names(ifs), mine)

	_, err = n.InterfaceByName(mine)
	assert.True(t, errors.Is(err, transport.ErrInterfaceNotFound), "got %v", err)

	host, err := net.InterfaceByName(mine)
	require.NoError(t, err)
	_, err = n.InterfaceByIndex(host.Index)
	assert.True(t, errors.Is(err, transport.ErrInterfaceNotFound), "got %v", err)
}

func TestRTCNetHidesNothingWithoutAMonitor(t *testing.T) {
	mine := hostInterface(t)
	for name, ctx := range map[string]context.Context{
		"no network manager": context.Background(),
		"no monitor": service.ContextWith[adapter.NetworkManager](context.Background(),
			&fakeNetworkManager{}),
	} {
		t.Run(name, func(t *testing.T) {
			n, err := newRTCNet(ctx, nil, nil)
			require.NoError(t, err)
			ifs, err := n.Interfaces()
			require.NoError(t, err)
			assert.Contains(t, names(ifs), mine)
			_, err = n.InterfaceByName(mine)
			assert.NoError(t, err)
		})
	}
}
