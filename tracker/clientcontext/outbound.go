package clientcontext

import (
	"context"
	"fmt"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	lconstant "github.com/getlantern/lantern-box/constant"
)

type outboundRegistry struct {
	adapter.OutboundRegistry
	injector *Injector
}

func (r *outboundRegistry) CreateOutbound(
	ctx context.Context,
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	outboundType string,
	options any,
) (adapter.Outbound, error) {
	out, err := r.OutboundRegistry.CreateOutbound(ctx, router, logger, tag, outboundType, options)
	if err != nil || !canWrapOutbound(out) {
		return out, err
	}
	return &outbound{Outbound: out, injector: r.injector}, nil
}

// Upstream exposes the wrapped registry, so protocols can still be registered
// on it after Install.
func (r *outboundRegistry) Upstream() any {
	return r.OutboundRegistry
}

// canWrapOutbound reports whether out's traffic flows only through DialContext and
// ListenPacket and out implements no interface that [outbound] would hide.
// Groups, including lantern-box's fallback, are excluded because they dial
// through member outbounds, which are wrapped themselves.
func canWrapOutbound(out adapter.Outbound) bool {
	if out.Type() == lconstant.TypeFallback || out.Type() == lconstant.TypeUnbounded {
		return false
	}
	switch out.(type) {
	case adapter.OutboundGroup,
		adapter.ConnectionHandlerEx,
		adapter.PacketConnectionHandlerEx,
		adapter.DirectRouteOutbound,
		adapter.OutboundWithPreferredRoutes,
		dialer.DirectDialer,
		dialer.ParallelInterfaceDialer,
		dialer.ParallelNetworkDialer,
		dialer.PacketDialerWithDestination:
		return false
	}
	return true
}

var (
	_ adapter.Outbound                = (*outbound)(nil)
	_ adapter.Lifecycle               = (*outbound)(nil)
	_ adapter.InterfaceUpdateListener = (*outbound)(nil)
)

// outbound sends the client-info frame on dials through an outbound enabled
// for injection: before returning the connection, or with its first write if
// the connection postpones its handshake.
type outbound struct {
	adapter.Outbound
	injector *Injector
}

func (o *outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	conn, err := o.Outbound.DialContext(ctx, network, destination)
	if err != nil || !o.injector.shouldInject(o.Tag()) {
		return conn, err
	}
	payload := o.injector.encodePayload(ctx)
	if N.NetworkName(network) == N.NetworkUDP {
		err = exchangePacket(ctx, bufio.NewUnbindPacketConn(conn), destination, payload)
	} else if early, ok := pendingHandshake(conn); ok {
		return newFrameConn(conn, early, payload), nil
	} else {
		err = exchangeStream(ctx, conn, payload)
	}
	if err != nil {
		return nil, fmt.Errorf("sending client info: %w", err)
	}
	return conn, nil
}

// pendingHandshake returns conn's EarlyConn if conn postpones its protocol
// handshake to the first write. Only such conns get a frameConn: wrapping one
// that has finished its handshake would report a pending one, so probes would
// stop counting the dial in their timing.
func pendingHandshake(conn net.Conn) (N.EarlyConn, bool) {
	early, ok := common.Cast[N.EarlyConn](conn)
	if !ok || !early.NeedHandshake() {
		return nil, false
	}
	return early, true
}

func (o *outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, err := o.Outbound.ListenPacket(ctx, destination)
	if err != nil || !o.injector.shouldInject(o.Tag()) {
		return conn, err
	}
	if err := exchangePacket(ctx, conn, destination, o.injector.encodePayload(ctx)); err != nil {
		return nil, fmt.Errorf("sending client info: %w", err)
	}
	return conn, nil
}

func (o *outbound) Start(stage adapter.StartStage) error {
	return adapter.LegacyStart(o.Outbound, stage)
}

func (o *outbound) Close() error {
	return common.Close(o.Outbound)
}

func (o *outbound) InterfaceUpdated() {
	if listener, ok := o.Outbound.(adapter.InterfaceUpdateListener); ok {
		listener.InterfaceUpdated()
	}
}

func (o *outbound) Upstream() any {
	return o.Outbound
}
