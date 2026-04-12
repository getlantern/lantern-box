package reflex

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"

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
// peer's certificate fingerprint as authentication.
type Inbound struct {
	inbound.Adapter
	ctx        context.Context
	logger     log.ContextLogger
	router     adapter.Router
	listener   *listener.Listener
	certFPs    map[string]bool // allowed SHA-256 cert fingerprints (lowercase hex)
	serverName string
}

func NewInbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.ReflexInboundOptions) (adapter.Inbound, error) {
	fps := make(map[string]bool, len(options.AuthTokens))
	for _, fp := range options.AuthTokens {
		normalized := strings.ToLower(strings.TrimSpace(fp))
		if len(normalized) != 64 {
			return nil, fmt.Errorf("reflex: invalid auth_tokens entry %q (expected 64 hex chars / SHA-256)", fp)
		}
		fps[normalized] = true
	}
	if len(fps) == 0 {
		return nil, fmt.Errorf("reflex: at least one auth_tokens entry (cert fingerprint) is required")
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
func (i *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	// Act as TLS client — send ClientHello immediately.
	// InsecureSkipVerify is intentional: the peer presents a self-signed cert
	// and we authenticate by validating its SHA-256 fingerprint against the
	// pre-shared list. Standard CA validation is not applicable here.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         i.serverName,
		InsecureSkipVerify: true, //nolint:gosec // auth via cert fingerprint, not CA chain
		MinVersion:         tls.VersionTLS13,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, fmt.Errorf("reflex: TLS handshake: %w", err))
		return
	}

	// Validate peer certificate fingerprint
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: no peer certificate"))
		return
	}
	fp := DERFingerprint(state.PeerCertificates[0].Raw)
	if !i.certFPs[fp] {
		i.logger.DebugContext(ctx, "reflex: unknown cert fingerprint: ", fp)
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: unauthorized"))
		return
	}

	// Read destination: 2-byte big-endian length + host:port string
	var lenBuf [2]byte
	if _, err := io.ReadFull(tlsConn, lenBuf[:]); err != nil {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: read destination length: %w", err))
		return
	}
	destLen := binary.BigEndian.Uint16(lenBuf[:])
	if destLen == 0 || destLen > 4096 {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: invalid destination length %d", destLen))
		return
	}
	destBuf := make([]byte, destLen)
	if _, err := io.ReadFull(tlsConn, destBuf); err != nil {
		N.CloseOnHandshakeFailure(tlsConn, onClose, fmt.Errorf("reflex: read destination: %w", err))
		return
	}
	destination := M.ParseSocksaddr(string(destBuf))

	i.logger.TraceContext(ctx, "reflex connection from ", metadata.Source, " to ", destination)

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
