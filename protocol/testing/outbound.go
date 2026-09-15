// Package testing implements the testing outbound, a direct egress dialer that
// connects to the destination it is handed.
package testing

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.TestingOutboundOptions](registry, constant.TypeTesting, NewOutbound)
}

// Outbound dials the destination it is handed through the configured dialer,
// relying on the box network manager's interface binding for the dial to leave
// via the real network rather than loop back into the TUN.
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	dialer N.Dialer
}

func NewOutbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.TestingOutboundOptions) (adapter.Outbound, error) {
	// The destination is supplied at dial time, so there is no configured
	// server address to resolve up front; remoteIsDomain is false.
	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			constant.TypeTesting,
			tag,
			[]string{N.NetworkTCP, N.NetworkUDP},
			options.DialerOptions,
		),
		logger: lg,
		dialer: outboundDialer,
	}, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	o.logger.TraceContext(ctx, "testing outbound dialing ", destination)
	return o.dialer.DialContext(ctx, network, destination)
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return o.dialer.ListenPacket(ctx, destination)
}

func (o *Outbound) Close() error {
	return nil
}
