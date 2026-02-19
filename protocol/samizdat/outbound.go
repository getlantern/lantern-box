package samizdat

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"

	samizdat "github.com/getlantern/samizdat"
)

// RegisterOutbound registers the Samizdat outbound adapter with the given registry.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.SamizdatOutboundOptions](registry, constant.TypeSamizdat, NewOutbound)
}

// Outbound represents a Samizdat outbound adapter.
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	client *samizdat.Client
}

// NewOutbound creates a new Samizdat outbound adapter.
func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	options option.SamizdatOutboundOptions,
) (adapter.Outbound, error) {
	// Decode hex public key
	pubKey, err := hex.DecodeString(options.PublicKey)
	if err != nil || len(pubKey) != 32 {
		return nil, fmt.Errorf("public_key must be 64 hex characters (32 bytes)")
	}

	// Decode hex short ID
	shortIDBytes, err := hex.DecodeString(options.ShortID)
	if err != nil || len(shortIDBytes) != 8 {
		return nil, fmt.Errorf("short_id must be 16 hex characters (8 bytes)")
	}
	var shortID [8]byte
	copy(shortID[:], shortIDBytes)

	// Build server address
	serverAddr := options.ServerOptions.Build().String()

	// Build sing-box dialer to honor DialerOptions (bind_interface, routing, detour, etc.)
	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, fmt.Errorf("creating dialer: %w", err)
	}

	// Parse timeouts
	var idleTimeout time.Duration
	if options.IdleTimeout != "" {
		idleTimeout, err = time.ParseDuration(options.IdleTimeout)
		if err != nil {
			return nil, fmt.Errorf("parsing idle_timeout: %w", err)
		}
	}

	var connectTimeout time.Duration
	if options.ConnectTimeout != "" {
		connectTimeout, err = time.ParseDuration(options.ConnectTimeout)
		if err != nil {
			return nil, fmt.Errorf("parsing connect_timeout: %w", err)
		}
	}

	config := samizdat.ClientConfig{
		ServerAddr:          serverAddr,
		ServerName:          options.ServerName,
		PublicKey:           pubKey,
		ShortID:             shortID,
		Fingerprint:         options.Fingerprint,
		Padding:             boolDefault(options.Padding, true),
		Jitter:              boolDefault(options.Jitter, true),
		MaxJitterMs:         options.MaxJitterMs,
		PaddingProfile:      options.PaddingProfile,
		TCPFragmentation:    boolDefault(options.TCPFragmentation, true),
		RecordFragmentation: boolDefault(options.RecordFragmentation, true),
		MaxStreamsPerConn:    options.MaxStreamsPerConn,
		IdleTimeout:         idleTimeout,
		ConnectTimeout:      connectTimeout,
		DataThreshold:       options.DataThreshold,
		Dialer: func(ctx context.Context, network, address string) (net.Conn, error) {
			return outboundDialer.DialContext(ctx, network, M.ParseSocksaddr(address))
		},
	}

	client, err := samizdat.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("creating samizdat client: %w", err)
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(
			constant.TypeSamizdat,
			tag,
			[]string{N.NetworkTCP},
			options.DialerOptions,
		),
		logger: logger,
		client: client,
	}, nil
}

// DialContext dials a connection to the destination through the Samizdat proxy.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination

	o.logger.InfoContext(ctx, "connecting to ", destination)
	conn, err := o.client.DialContext(ctx, network, destination.String())
	if err != nil {
		return nil, fmt.Errorf("samizdat dial to %s: %w", destination, err)
	}

	return conn, nil
}

// ListenPacket is not supported by Samizdat (TCP-only protocol).
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("samizdat does not support UDP")
}

// Network returns the supported network types.
func (o *Outbound) Network() []string {
	return []string{N.NetworkTCP}
}

// Close shuts down the Samizdat client.
func (o *Outbound) Close() error {
	if o.client != nil {
		return o.client.Close()
	}
	return nil
}

// boolDefault returns the value pointed to by p, or defaultVal if p is nil.
func boolDefault(p *bool, defaultVal bool) bool {
	if p != nil {
		return *p
	}
	return defaultVal
}
