package samizdat

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/uot"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"

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
	ctx         context.Context
	logger      log.ContextLogger
	router      adapter.ConnectionRouterEx
	listener    *listener.Listener
	tcpListener net.Listener
	server      *samizdat.Server
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

	if len(options.ShortIDs) < 1 {
		return nil, fmt.Errorf("short_ids must contain at least one element")
	}

	// Decode hex short IDs
	shortIDs := make([][8]byte, len(options.ShortIDs))
	for i, sid := range options.ShortIDs {
		b, decodeErr := hex.DecodeString(sid)
		if decodeErr != nil || len(b) != 8 {
			return nil, fmt.Errorf("short_ids[%d] must be 16 hex characters (8 bytes)", i)
		}
		copy(shortIDs[i][:], b)
	}

	// Validate and load TLS certificate/key pair
	var certPEM, keyPEM []byte
	if options.CertPEM != "" || options.KeyPEM != "" {
		if options.CertPEM == "" || options.KeyPEM == "" {
			return nil, fmt.Errorf("both cert_pem and key_pem must be provided when using inline PEM")
		}
		if options.CertPath != "" || options.KeyPath != "" {
			return nil, fmt.Errorf("cannot mix inline PEM (cert_pem/key_pem) with file paths (cert_path/key_path)")
		}
		certPEM = []byte(options.CertPEM)
		keyPEM = []byte(options.KeyPEM)
	} else if options.CertPath != "" || options.KeyPath != "" {
		if options.CertPath == "" || options.KeyPath == "" {
			return nil, fmt.Errorf("both cert_path and key_path must be provided when using file paths")
		}
		certPEM, err = os.ReadFile(options.CertPath)
		if err != nil {
			return nil, fmt.Errorf("reading cert file: %w", err)
		}
		keyPEM, err = os.ReadFile(options.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading key file: %w", err)
		}
	} else {
		return nil, fmt.Errorf("TLS certificate and key must be provided via cert_pem/key_pem or cert_path/key_path")
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

	ib := &Inbound{
		Adapter: inbound.NewAdapter(constant.TypeSamizdat, tag),
		ctx:     ctx,
		logger:  logger,
		router:  uot.NewRouter(router, logger),
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
	}

	// Use sing-box's listener to create the TCP listener, honoring ListenOptions
	tcpListener, err := ib.listener.ListenTCP()
	if err != nil {
		return nil, fmt.Errorf("creating TCP listener: %w", err)
	}
	ib.tcpListener = tcpListener

	serverConfig := samizdat.ServerConfig{
		PrivateKey:            privKey,
		ShortIDs:              shortIDs,
		CertPEM:               certPEM,
		KeyPEM:                keyPEM,
		MasqueradeDomain:      options.MasqueradeDomain,
		MasqueradeAddr:        options.MasqueradeAddr,
		MasqueradeIdleTimeout: masqIdleTimeout,
		MasqueradeMaxDuration: masqMaxDuration,
		MaxConcurrentStreams:  options.MaxConcurrentStreams,
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
// sing-box router. It blocks until the proxied connection is complete,
// which is required because the samizdat H2 handler closes the stream
// when the Handler function returns.
func (i *Inbound) handleConnection(ctx context.Context, conn net.Conn, destination string) {
	var metadata adapter.InboundContext
	metadata.Inbound = i.Tag()
	metadata.InboundType = i.Type()
	metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr()).Unwrap()
	metadata.Destination = M.ParseSocksaddr(destination)

	i.logger.InfoContext(ctx, "inbound connection to ", destination)
	done := make(chan struct{})
	i.router.RouteConnectionEx(ctx, conn, metadata, func(err error) {
		if err != nil {
			i.logger.ErrorContext(ctx, err)
		}
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
		i.logger.ErrorContext(ctx, "inbound connection to ", destination, " canceled: ", ctx.Err())
	}
}

// Start starts the Samizdat inbound server.
func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}

	go func() {
		if err := i.server.Serve(i.tcpListener); err != nil {
			i.logger.Error("samizdat server error: ", err)
		}
	}()

	return nil
}

// Close stops the Samizdat inbound server and sing-box listener.
func (i *Inbound) Close() error {
	var serverErr, listenerErr error
	if i.server != nil {
		serverErr = i.server.Close()
	}
	if i.listener != nil {
		listenerErr = i.listener.Close()
	}
	return errors.Join(serverErr, listenerErr)
}
