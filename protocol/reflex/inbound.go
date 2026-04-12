package reflex

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
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
// immediately sends a TLS ClientHello (reversed role), then validates the
// client's TLS certificate fingerprint as authentication. No pre-handshake
// bytes are exchanged — the entire flow looks like a TLS connection where
// the server speaks first.
type Inbound struct {
	inbound.Adapter
	ctx            context.Context
	logger         log.ContextLogger
	router         adapter.Router
	listener       *listener.Listener
	certFPs        map[string]bool // allowed SHA-256 cert fingerprints (hex)
	serverName     string
}

func NewInbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.ReflexInboundOptions) (adapter.Inbound, error) {
	// Parse allowed certificate fingerprints
	fps := make(map[string]bool, len(options.AuthTokens))
	for _, fp := range options.AuthTokens {
		fps[fp] = true
	}
	if len(fps) == 0 {
		return nil, fmt.Errorf("reflex: at least one auth_token (cert fingerprint) is required")
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
		certFPs:    fps,
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
// The server immediately acts as TLS client — no pre-handshake bytes needed.
func (i *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	// 1. Act as TLS client — send ClientHello immediately.
	// Request the peer's certificate for authentication.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         i.serverName,
		InsecureSkipVerify: true,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, fmt.Errorf("reflex: TLS handshake: %w", err))
		return
	}

	// 2. Validate the peer's certificate fingerprint
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: no peer certificate"))
		return
	}
	hash := sha256.Sum256(state.PeerCertificates[0].Raw)
	fp := fmt.Sprintf("%x", hash)
	if !i.certFPs[fp] {
		i.logger.DebugContext(ctx, "reflex: unknown cert fingerprint: ", fp)
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: unauthorized"))
		return
	}

	// 3. Read destination: 1 byte length + host:port string
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

	// 4. Route through sing-box
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
