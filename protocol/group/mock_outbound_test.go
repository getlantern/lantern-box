package group

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/mock"
)

type mockOutbound struct {
	mock.Mock
	adapter.Outbound
	tag string
}

func (m *mockOutbound) Tag() string       { return m.tag }
func (m *mockOutbound) Network() []string { return []string{"tcp", "udp"} }

func (m *mockOutbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	args := m.Called()
	conn, _ := args.Get(0).(net.Conn)
	return conn, args.Error(1)
}

func (m *mockOutbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	args := m.Called()
	pc, _ := args.Get(0).(net.PacketConn)
	return pc, args.Error(1)
}

type mockOutboundManager struct {
	mock.Mock
	adapter.OutboundManager
	outbounds map[string]adapter.Outbound
}

func (m *mockOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	o, ok := m.outbounds[tag]
	return o, ok
}
