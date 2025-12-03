package group

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterFallback(registry *outbound.Registry) {
	outbound.Register[option.FallbackOutboundOptions](registry, constant.TypeFallback, NewFallback)
}

// Fallback is an outbound adapter that attempts to use a primary outbound, and falls back to a
// secondary outbound if the primary fails.
type Fallback struct {
	outbound.Adapter
	outboundMgr adapter.OutboundManager
	logger      logger.ContextLogger
	primaryTag  string
	fallbackTag string
}

// NewFallback creates a new Fallback outbound adapter.
func NewFallback(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.FallbackOutboundOptions) (adapter.Outbound, error) {
	if options.Primary == "" {
		return nil, errors.New("missing primary outbound tag")
	}
	if options.Fallback == "" {
		return nil, errors.New("missing fallback outbound tag")
	}
	if options.Primary == options.Fallback {
		return nil, fmt.Errorf("primary and fallback outbound tags cannot be the same: %s", options.Primary)
	}

	outbounds := []string{options.Primary, options.Fallback}
	return &Fallback{
		Adapter:     outbound.NewAdapter(constant.TypeFallback, tag, nil, outbounds),
		outboundMgr: service.FromContext[adapter.OutboundManager](ctx),
		logger:      logger,
		primaryTag:  options.Primary,
		fallbackTag: options.Fallback,
	}, nil
}

// Start checks that both the primary and fallback outbounds are available.
func (d *Fallback) Start() error {
	if _, loaded := d.outboundMgr.Outbound(d.primaryTag); !loaded {
		return fmt.Errorf("missing primary outbound %s", d.primaryTag)
	}
	if _, loaded := d.outboundMgr.Outbound(d.fallbackTag); !loaded {
		return fmt.Errorf("missing fallback outbound %s", d.fallbackTag)
	}
	return nil
}

// DialContext attempts to dial using the primary outbound. If it fails, it falls back to the
// fallback outbound.
func (d *Fallback) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	outbound, loaded := d.outboundMgr.Outbound(d.primaryTag)
	if !loaded {
		return nil, fmt.Errorf("primary outbound %s not found", d.primaryTag)
	}
	conn, err := outbound.DialContext(ctx, network, destination)
	if err == nil {
		return conn, nil
	}

	d.logger.WarnContext(ctx, "dial on primary outbound: ", err)
	outbound, loaded = d.outboundMgr.Outbound(d.fallbackTag)
	if !loaded {
		return nil, fmt.Errorf("fallback outbound %s not found", d.fallbackTag)
	}
	fallbackConn, err := outbound.DialContext(ctx, network, destination)
	if err != nil {
		return nil, fmt.Errorf("dial on fallback outbound: %w", err)
	}
	return fallbackConn, nil
}

// ListenPacket attempts to create a packet connection using the primary outbound. If it fails, it
// falls back to the fallback outbound.
func (d *Fallback) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	outbound, loaded := d.outboundMgr.Outbound(d.primaryTag)
	if !loaded {
		return nil, fmt.Errorf("primary outbound %s not found", d.primaryTag)
	}
	conn, err := outbound.ListenPacket(ctx, destination)
	if err == nil {
		return conn, nil
	}

	d.logger.WarnContext(ctx, "packet connection on primary outbound: ", err)
	outbound, loaded = d.outboundMgr.Outbound(d.fallbackTag)
	if !loaded {
		return nil, fmt.Errorf("fallback outbound %s not found", d.fallbackTag)
	}
	fallbackConn, err := outbound.ListenPacket(ctx, destination)
	if err != nil {
		return nil, fmt.Errorf("packet connection on fallback outbound: %w", err)
	}
	return fallbackConn, nil
}
