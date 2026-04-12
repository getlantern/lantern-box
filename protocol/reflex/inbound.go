package reflex

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.ReflexInboundOptions](registry, constant.TypeReflex, NewInbound)
}

// Inbound implements the server side of Reflex. It accepts TCP connections,
// reads an auth token, then acts as a TLS client (sends ClientHello to the
// TCP client). After the reversed TLS handshake, it reads the destination
// and routes traffic through the sing-box router.
type Inbound struct {
	inbound.Adapter
	ctx        context.Context
	logger     log.ContextLogger
	router     adapter.Router
	listener   *listener.Listener
	authTokens map[string]bool
	serverName string
}

func NewInbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.ReflexInboundOptions) (adapter.Inbound, error) {
	tokens := make(map[string]bool, len(options.AuthTokens))
	for _, t := range options.AuthTokens {
		b, err := hex.DecodeString(t)
		if err != nil || len(b) != 8 {
			return nil, fmt.Errorf("reflex: invalid auth_token %q (must be 16 hex chars)", t)
		}
		tokens[string(b)] = true
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("reflex: at least one auth_token is required")
	}

	serverName := options.ServerName
	if serverName == "" {
		serverName = "www.example.com"
	}

	ib := &Inbound{
		Adapter:    inbound.NewAdapter(constant.TypeReflex, tag),
		ctx:        ctx,
		logger:     lg,
		router:     router,
		authTokens: tokens,
		serverName: serverName,
	}

	ib.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            lg,
		Network:           []string{N.NetworkTCP},
		Listen:            options.ListenOptions,
		ConnectionHandler: ib,
	})

	return ib, nil
}

// NewConnectionEx handles a new TCP connection with the reversed TLS handshake.
func (i *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	// 1. Read 8-byte auth token
	token := make([]byte, 8)
	if _, err := io.ReadFull(conn, token); err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, fmt.Errorf("reflex: read auth token: %w", err))
		return
	}

	// 2. Validate token
	if !i.authTokens[string(token)] {
		slog.Debug("reflex: invalid auth token, closing connection")
		N.CloseOnHandshakeFailure(conn, onClose, fmt.Errorf("reflex: invalid auth token"))
		return
	}

	// 3. Act as TLS client — send ClientHello to the TCP client
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         i.serverName,
		InsecureSkipVerify: true,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, fmt.Errorf("reflex: TLS handshake: %w", err))
		return
	}

	// 4. Read destination: 1 byte length + host:port string
	lenBuf := make([]byte, 1)
	if _, err := io.ReadFull(tlsConn, lenBuf); err != nil {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: read destination length: %w", err))
		return
	}
	destBuf := make([]byte, lenBuf[0])
	if _, err := io.ReadFull(tlsConn, destBuf); err != nil {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: read destination: %w", err))
		return
	}
	destination := M.ParseSocksaddr(string(destBuf))

	i.logger.TraceContext(ctx, "reflex connection from ", metadata.Source, " to ", destination)

	// 5. Route through sing-box
	metadata.Inbound = i.Tag()
	metadata.InboundType = constant.TypeReflex
	metadata.Destination = destination

	i.router.RouteConnectionEx(ctx, tlsConn, metadata, onClose)
}

func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	return i.listener.Start()
}

func (i *Inbound) Close() error {
	if i.listener != nil {
		return i.listener.Close()
	}
	return nil
}
