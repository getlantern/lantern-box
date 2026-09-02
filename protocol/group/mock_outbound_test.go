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
	tag      string
	typeName string
	networks []string
	// dial, when set, supersedes the mock expectation so each call gets
	// its own conn; probe.Run closes what it dials, so a shared one breaks
	// on the second probe. It receives the caller's context so a stub can
	// honor the probe deadline the way a real outbound does.
	dial func(context.Context) (net.Conn, error)
}

func (m *mockOutbound) Tag() string { return m.tag }
func (m *mockOutbound) Network() []string {
	if m.networks != nil {
		return m.networks
	}
	return []string{"tcp", "udp"}
}

func (m *mockOutbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	if m.dial != nil {
		return m.dial(ctx)
	}
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
