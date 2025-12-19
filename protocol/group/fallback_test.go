package group

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type mockOutbound struct {
	mock.Mock
	adapter.Outbound
	tag string
}

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

func newTestFB() (*Fallback, *mockOutbound, *mockOutbound) {
	primary := new(mockOutbound)
	fallback := new(mockOutbound)
	outboundMgr := &mockOutboundManager{
		outbounds: map[string]adapter.Outbound{
			"primary":  primary,
			"fallback": fallback,
		},
	}
	return &Fallback{
		outboundMgr: outboundMgr,
		logger:      log.NewNOPFactory().Logger(),
		primaryTag:  "primary",
		fallbackTag: "fallback",
	}, primary, fallback
}

func TestFallback(t *testing.T) {
	ctx := context.Background()

	for _, method := range []string{"DialContext", "ListenPacket"} {
		t.Run(method+": success on primary", func(t *testing.T) {
			f, primary, fallback := newTestFB()
			primary.On(method).Return(&net.IPConn{}, nil)
			switch method {
			case "DialContext":
				conn, err := f.DialContext(ctx, "tcp", metadata.Socksaddr{})
				assert.NoError(t, err)
				assert.NotNil(t, conn)
			case "ListenPacket":
				conn, err := f.ListenPacket(ctx, metadata.Socksaddr{})
				assert.NoError(t, err)
				assert.NotNil(t, conn)
			}
			primary.AssertExpectations(t)
			fallback.AssertNotCalled(t, method)
		})
		t.Run(method+": primary fails, success on fallback", func(t *testing.T) {
			f, primary, fallback := newTestFB()
			primary.On(method).Return(nil, assert.AnError)
			fallback.On(method).Return(&net.IPConn{}, nil)
			switch method {
			case "DialContext":
				conn, err := f.DialContext(ctx, "tcp", metadata.Socksaddr{})
				assert.NoError(t, err)
				assert.NotNil(t, conn)
			case "ListenPacket":
				conn, err := f.ListenPacket(ctx, metadata.Socksaddr{})
				assert.NoError(t, err)
				assert.NotNil(t, conn)
			}
			primary.AssertExpectations(t)
			fallback.AssertExpectations(t)
		})
		t.Run(method+": primary and fallback fail", func(t *testing.T) {
			f, primary, fallback := newTestFB()
			primary.On(method).Return(nil, assert.AnError)
			fallback.On(method).Return(nil, assert.AnError)
			switch method {
			case "DialContext":
				conn, err := f.DialContext(ctx, "tcp", metadata.Socksaddr{})
				assert.Error(t, err)
				assert.Nil(t, conn)
			case "ListenPacket":
				conn, err := f.ListenPacket(ctx, metadata.Socksaddr{})
				assert.Error(t, err)
				assert.Nil(t, conn)
			}
			primary.AssertExpectations(t)
			fallback.AssertExpectations(t)
		})
	}
}
