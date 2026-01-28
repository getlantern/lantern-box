package metrics

import (
	"context"
	"net"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
)

var _ (adapter.ConnectionTracker) = (*MetricsTracker)(nil)

type MetricsTracker struct{}

func NewTracker() (*MetricsTracker, error) {
	return &MetricsTracker{}, nil
}

func (t *MetricsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if ok {
		go countDistinctClients(info.DeviceID)
	}
	return NewConn(conn, &metadata)
}

func (t *MetricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if ok {
		go countDistinctClients(info.DeviceID)
	}
	return NewPacketConn(conn, &metadata)
}
