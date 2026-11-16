package physnet

import (
	"context"
	"syscall"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNetworkManager counts the calls of its bind and auto-redirect mark controls.
type fakeNetworkManager struct {
	adapter.NetworkManager
	mark  uint32
	binds int
	marks int
}

func (m *fakeNetworkManager) ProtectFunc() control.Func      { return nil }
func (m *fakeNetworkManager) AutoRedirectOutputMark() uint32 { return m.mark }
func (m *fakeNetworkManager) AutoDetectInterfaceFunc() control.Func {
	return func(string, string, syscall.RawConn) error { m.binds++; return nil }
}
func (m *fakeNetworkManager) AutoRedirectOutputMarkFunc() control.Func {
	return func(string, string, syscall.RawConn) error { m.marks++; return nil }
}

func TestListenControlMarksForAutoRedirect(t *testing.T) {
	nm := &fakeNetworkManager{mark: 0x2023}
	f := ListenControl(service.ContextWith[adapter.NetworkManager](context.Background(), nm))
	require.NotNil(t, f)
	require.NoError(t, f("udp4", "0.0.0.0:0", nil))
	assert.Equal(t, 1, nm.binds)
	assert.Equal(t, 1, nm.marks, "auto_redirect's OUTPUT rules need the mark to skip this socket")
}

func TestListenControlWithoutAutoRedirect(t *testing.T) {
	nm := &fakeNetworkManager{}
	f := ListenControl(service.ContextWith[adapter.NetworkManager](context.Background(), nm))
	require.NotNil(t, f)
	require.NoError(t, f("udp4", "0.0.0.0:0", nil))
	assert.Equal(t, 1, nm.binds)
	assert.Zero(t, nm.marks)
}

func TestListenControlWithoutAVPN(t *testing.T) {
	assert.Nil(t, ListenControl(context.Background()))
}
