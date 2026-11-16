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
	mark        uint32
	defaultMark uint32
	binds       int
	marks       int
}

func (m *fakeNetworkManager) DefaultOptions() adapter.NetworkOptions {
	return adapter.NetworkOptions{RoutingMark: m.defaultMark}
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

// recordMarks swaps routingMark for one that records the marks it is asked to set.
func recordMarks(t *testing.T) *[]uint32 {
	var set []uint32
	orig := routingMark
	routingMark = func(mark uint32) control.Func {
		return func(string, string, syscall.RawConn) error { set = append(set, mark); return nil }
	}
	t.Cleanup(func() { routingMark = orig })
	return &set
}

func TestListenControlAppliesDefaultMark(t *testing.T) {
	set := recordMarks(t)
	nm := &fakeNetworkManager{defaultMark: 0x1f}
	f := ListenControl(service.ContextWith[adapter.NetworkManager](context.Background(), nm))
	require.NoError(t, f("udp4", "0.0.0.0:0", nil))
	assert.Equal(t, []uint32{0x1f}, *set, "route.default_mark, as sing-box's default dialer sets it")
	assert.Equal(t, 1, nm.binds)
}

func TestListenControlDefaultMarkConflictsWithAutoRedirect(t *testing.T) {
	set := recordMarks(t)
	nm := &fakeNetworkManager{defaultMark: 0x1f, mark: 0x2023}
	f := ListenControl(service.ContextWith[adapter.NetworkManager](context.Background(), nm))
	require.Error(t, f("udp4", "0.0.0.0:0", nil), "sing-box's default dialer refuses both marks")
	assert.Empty(t, *set)
	assert.Zero(t, nm.marks)
}

func TestListenControlWithoutDefaultMark(t *testing.T) {
	set := recordMarks(t)
	nm := &fakeNetworkManager{}
	f := ListenControl(service.ContextWith[adapter.NetworkManager](context.Background(), nm))
	require.NoError(t, f("udp4", "0.0.0.0:0", nil))
	assert.Empty(t, *set)
}
