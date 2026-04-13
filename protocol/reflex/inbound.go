package reflex

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// defaultSilenceJitter is applied when SilenceTimeout is set but
// SilenceJitter is not explicitly configured.
const defaultSilenceJitter = 2 * time.Second

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.ReflexInboundOptions](registry, constant.TypeReflex, NewInbound)
}

// Inbound implements the server side of Reflex. It accepts TCP connections,
// immediately sends a TLS ClientHello (reversed role), then validates the
// peer's certificate fingerprint as authentication.
type Inbound struct {
	inbound.Adapter
	ctx                context.Context
	logger             log.ContextLogger
	router             adapter.Router
	listener           *listener.Listener
	certFPs            map[string]bool // allowed SHA-256 cert fingerprints (lowercase hex)
	serverName         string
	silenceTimeout     time.Duration // 0 = disabled
	silenceJitter      time.Duration
	masqueradeUpstream string
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

	var silenceTimeout, silenceJitter time.Duration
	if options.SilenceTimeout != "" {
		d, err := time.ParseDuration(options.SilenceTimeout)
		if err != nil {
			return nil, fmt.Errorf("reflex: invalid silence_timeout: %w", err)
		}
		if d < 0 {
			return nil, fmt.Errorf("reflex: silence_timeout must be non-negative, got %s", d)
		}
		silenceTimeout = d
	}
	if silenceTimeout > 0 {
		if options.MasqueradeUpstream == "" {
			return nil, fmt.Errorf("reflex: masquerade_upstream is required when silence_timeout is set")
		}
		if options.SilenceJitter != "" {
			d, err := time.ParseDuration(options.SilenceJitter)
			if err != nil {
				return nil, fmt.Errorf("reflex: invalid silence_jitter: %w", err)
			}
			if d < 0 {
				return nil, fmt.Errorf("reflex: silence_jitter must be non-negative, got %s", d)
			}
			silenceJitter = d
		} else {
			silenceJitter = defaultSilenceJitter
		}
		// Minimum wait is silence_timeout - silence_jitter. If jitter >=
		// timeout, the minimum wait can reach zero and effectively skip the
		// silence window — undermining probe resistance. Reject that config.
		if silenceJitter >= silenceTimeout {
			return nil, fmt.Errorf("reflex: silence_jitter (%s) must be less than silence_timeout (%s)", silenceJitter, silenceTimeout)
		}
	}

	ib := &Inbound{
		Adapter:            inbound.NewAdapter(constant.TypeReflex, tag),
		ctx:                ctx,
		logger:             lg,
		router:             router,
		certFPs:            fps,
		serverName:         serverName,
		silenceTimeout:     silenceTimeout,
		silenceJitter:      silenceJitter,
		masqueradeUpstream: options.MasqueradeUpstream,
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
	// Silence-based probe resistance: a legitimate Lantern client sends no bytes
	// until the server speaks (that's the Reflex protocol). Active probes and
	// misdirected TLS clients speak immediately. If we see any bytes within the
	// silence window, forward the connection to the masquerade upstream instead
	// of revealing Reflex.
	if i.silenceTimeout > 0 {
		wait := jitteredTimeout(i.silenceTimeout, i.silenceJitter)
		prefix, err := waitForSilence(conn, wait)
		if err != nil {
			i.logger.DebugContext(ctx, "reflex: silence read error: ", err)
			N.CloseOnHandshakeFailure(conn, onClose, err)
			return
		}
		if len(prefix) > 0 {
			i.logger.DebugContext(ctx, "reflex: client spoke during silence window from ", metadata.Source, "; masquerading to ", i.masqueradeUpstream)
			ferr := forwardToMasquerade(ctx, conn, i.masqueradeUpstream, prefix)
			if ferr != nil {
				i.logger.DebugContext(ctx, "reflex: masquerade forward error: ", ferr)
			}
			conn.Close()
			if onClose != nil {
				onClose(ferr)
			}
			return
		}
	}

	// Act as TLS client — send ClientHello immediately.
	// InsecureSkipVerify is intentional: the peer presents a self-signed cert
	// and we authenticate by validating its SHA-256 fingerprint against the
	// pre-shared list. Standard CA validation is not applicable here.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         i.serverName,
		InsecureSkipVerify: true, //nolint:gosec // auth via cert fingerprint, not CA chain
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
