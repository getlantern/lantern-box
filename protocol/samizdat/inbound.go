package samizdat

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"

	samizdat "github.com/getlantern/samizdat"
)

// RegisterInbound registers the Samizdat inbound adapter with the given registry.
func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.SamizdatInboundOptions](registry, constant.TypeSamizdat, NewInbound)
}

// Inbound represents a Samizdat inbound adapter.
type Inbound struct {
	inbound.Adapter
	ctx    context.Context
	logger log.ContextLogger
	router adapter.Router
	server *samizdat.Server
}

// NewInbound creates a new Samizdat inbound adapter.
func NewInbound(
	ctx context.Context,
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	options option.SamizdatInboundOptions,
) (adapter.Inbound, error) {
	// Decode hex private key
	privKey, err := hex.DecodeString(options.PrivateKey)
	if err != nil || len(privKey) != 32 {
		return nil, fmt.Errorf("private_key must be 64 hex characters (32 bytes)")
	}

	// Decode hex short IDs
	shortIDs := make([][8]byte, len(options.ShortIDs))
	for i, sid := range options.ShortIDs {
		b, err := hex.DecodeString(sid)
		if err != nil || len(b) != 8 {
			return nil, fmt.Errorf("short_ids[%d] must be 16 hex characters (8 bytes)", i)
		}
		copy(shortIDs[i][:], b)
	}

	// Load TLS certificate
	var certPEM, keyPEM []byte
	if options.CertPEM != "" {
		certPEM = []byte(options.CertPEM)
		keyPEM = []byte(options.KeyPEM)
	} else if options.CertPath != "" {
		certPEM, err = os.ReadFile(options.CertPath)
		if err != nil {
			return nil, fmt.Errorf("reading cert file: %w", err)
		}
		keyPEM, err = os.ReadFile(options.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading key file: %w", err)
		}
	}

	// Parse timeouts
	var masqIdleTimeout, masqMaxDuration time.Duration
	if options.MasqueradeIdleTimeout != "" {
		masqIdleTimeout, err = time.ParseDuration(options.MasqueradeIdleTimeout)
		if err != nil {
			return nil, fmt.Errorf("parsing masquerade_idle_timeout: %w", err)
		}
	}
	if options.MasqueradeMaxDuration != "" {
		masqMaxDuration, err = time.ParseDuration(options.MasqueradeMaxDuration)
		if err != nil {
			return nil, fmt.Errorf("parsing masquerade_max_duration: %w", err)
		}
	}

	// Build listen address from ListenOptions
	listenAddr := fmt.Sprintf(":%d", options.ListenPort)
	if options.Listen != nil {
		listenAddr = fmt.Sprintf("%s:%d", netip.Addr(*options.Listen).String(), options.ListenPort)
	}

	ib := &Inbound{
		Adapter: inbound.NewAdapter(constant.TypeSamizdat, tag),
		ctx:     ctx,
		logger:  logger,
		router:  router,
	}

	serverConfig := samizdat.ServerConfig{
		ListenAddr:            listenAddr,
		PrivateKey:            privKey,
		ShortIDs:              shortIDs,
		CertPEM:               certPEM,
		KeyPEM:                keyPEM,
		MasqueradeDomain:      options.MasqueradeDomain,
		MasqueradeAddr:        options.MasqueradeAddr,
		MasqueradeIdleTimeout: masqIdleTimeout,
		MasqueradeMaxDuration: masqMaxDuration,
		MaxConcurrentStreams:   options.MaxConcurrentStreams,
		Handler: func(ctx context.Context, conn net.Conn, destination string) {
			ib.handleConnection(ctx, conn, destination)
		},
	}

	server, err := samizdat.NewServer(serverConfig)
	if err != nil {
		return nil, fmt.Errorf("creating samizdat server: %w", err)
	}
	ib.server = server

	return ib, nil
}

// handleConnection routes authenticated proxied connections through the
// sing-box router.
func (i *Inbound) handleConnection(ctx context.Context, conn net.Conn, destination string) {
	var metadata adapter.InboundContext
	metadata.Inbound = i.Tag()
	metadata.InboundType = i.Type()
	metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr()).Unwrap()
	metadata.Destination = M.ParseSocksaddr(destination)

	i.logger.InfoContext(ctx, "inbound connection to ", destination)
	i.router.RouteConnectionEx(ctx, conn, metadata, func(err error) {
		if err != nil {
			i.logger.ErrorContext(ctx, err)
		}
	})
}

func (i *Inbound) newPacketConnection(ctx context.Context, conn network.PacketConn, metadata adapter.InboundContext, onClose network.CloseHandlerFunc) {
	i.logger.ErrorContext(ctx, "packet connection not supported by samizdat")
}

// Start starts the Samizdat inbound server.
func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}

	go func() {
		if err := i.server.ListenAndServe(); err != nil {
			i.logger.Error("samizdat server error: ", err)
		}
	}()

	return nil
}

// Close stops the Samizdat inbound server.
func (i *Inbound) Close() error {
	if i.server != nil {
		return i.server.Close()
	}
	return nil
}
