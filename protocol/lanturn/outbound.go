// Package lanturn implements the client-side lanturn outbound for
// sing-box.
//
// lanturn is a Lantern circumvention transport that mimics WebRTC
// TURN-relayed media flow on plain UDP/3478 with self-hosted coturn
// on Lantern's international VPS fleet. See
// [getlantern/lanturn](https://github.com/getlantern/lanturn) for the
// protocol details + the design draft in the
// circumvention-corpus-private repo (private).
//
// # Architecture
//
// The outbound calls [lanturn.Dial] at Start time to establish a
// persistent connection to a Lantern egress process colocated with a
// coturn instance on a Lantern VPS. Bytes flow:
//
//	caller bytes
//	  → SRTP-paced chunks at the chosen MediaProfile cadence
//	  → AES-128-CM-HMAC-SHA1-80 (SRTP)
//	  → DTLS-derived keying material (RFC 5764 §4.2)
//	  → TURN ChannelData wrapping
//	  → Plain UDP/3478 to coturn (or TURNS-on-TCP/5349 fallback)
//
// The egress is the lanturn server listening on the same VPS as
// coturn. It receives client bytes on the UDP path coturn relays into
// it; from the egress's perspective, lanturn looks like a SOCKS-shaped
// proxy and it forwards bytes onward to the user's destination.
//
// # Multiplexing
//
// MVP opens a fresh [lanturn.Dial] PER outbound DialContext call —
// one TURN allocation + DTLS handshake + SRTP key set per TCP dial.
// This is heavy but simple and matches the spike's behavior. Production
// follow-up work is to maintain a long-lived lanturn session and
// multiplex destinations via a SOCKS5 control protocol on top, the
// same pattern Unbounded uses (the consumer-side outbound holds one
// QUIC-over-WebRTC session and SOCKS5-CONNECTs per destination).
package lanturn

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	C "github.com/getlantern/lantern-box/constant"
	lbopt "github.com/getlantern/lantern-box/option"

	upstream "github.com/getlantern/lanturn/pkg/lanturn"
)

// RegisterOutbound registers the lanturn outbound with the given
// sing-box OutboundRegistry. The hosting process should call this once
// at startup, before libbox.NewServiceWithContext.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[lbopt.LanturnOutboundOptions](registry, C.TypeLanturn, NewOutbound)
}

// Outbound is the client-side lanturn outbound.
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	cfg    upstream.ClientConfig
}

func NewOutbound(
	ctx context.Context,
	router adapter.Router,
	lg sblog.ContextLogger,
	tag string,
	opts lbopt.LanturnOutboundOptions,
) (adapter.Outbound, error) {
	if len(opts.CoturnEndpoints) == 0 {
		return nil, fmt.Errorf("lanturn: at least one coturn endpoint required")
	}
	if opts.PeerAddr == "" {
		return nil, fmt.Errorf("lanturn: peer_addr required")
	}
	if opts.LanturnAuthSecret == "" {
		return nil, fmt.Errorf("lanturn: lanturn_auth_secret required (v0.1; production should use Lantern config service)")
	}

	endpoints := make([]upstream.CoturnEndpoint, len(opts.CoturnEndpoints))
	for i, ep := range opts.CoturnEndpoints {
		endpoints[i] = upstream.CoturnEndpoint{
			UDPAddr:    ep.UDPAddr,
			TLSAddr:    ep.TLSAddr,
			ServerName: ep.ServerName,
		}
	}

	cfg := upstream.ClientConfig{
		CoturnEndpoints: endpoints,
		PeerAddr:        opts.PeerAddr,
		Profile:         upstream.MediaProfile(opts.Profile),
		// Credential: v0.1 derives the OAUTH cred from the static
		// auth secret. Production: replace with a callback to
		// Lantern's config service.
		Credential: func(ep upstream.CoturnEndpoint) (upstream.Credential, error) {
			return upstream.Credential{
				// MVP shortcut: pkg/lanturn's internal/turn package
				// generates the OAUTH cred itself from the static
				// secret, so we pass the secret as the password
				// field. See pkg/lanturn comments for the planned
				// production refactor.
				Username: "",
				Password: opts.LanturnAuthSecret,
			}, nil
		},
		Logger: func(format string, args ...any) {
			lg.DebugContext(context.Background(), fmt.Sprintf(format, args...))
		},
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(C.TypeLanturn, tag, []string{N.NetworkTCP, N.NetworkUDP}, opts.DialerOptions),
		logger:  lg,
		cfg:     cfg,
	}, nil
}

// DialContext opens a lanturn-tunneled TCP connection. MVP allocates a
// fresh TURN session per dial; production should multiplex over a
// persistent session.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	switch N.NetworkName(network) {
	case N.NetworkTCP:
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		o.logger.DebugContext(ctx, "lanturn TCP dial start", "dest", destination.String())
		conn, err := upstream.Dial(dialCtx, o.cfg)
		if err != nil {
			o.logger.ErrorContext(ctx, "lanturn TCP dial failed", "dest", destination.String(), "err", err)
			return nil, fmt.Errorf("lanturn dial: %w", err)
		}
		// PHASE-5-TODO: send SOCKS5 CONNECT(destination) handshake
		// to the egress over the lanturn conn so the egress knows
		// where to forward bytes. For MVP, the egress acts as a
		// transparent forwarder to a single hardcoded destination.
		o.logger.DebugContext(ctx, "lanturn TCP dial ok", "dest", destination.String())
		return conn, nil
	case N.NetworkUDP:
		return nil, fmt.Errorf("lanturn: UDP destinations not yet supported (MVP TCP only)")
	}
	return nil, fmt.Errorf("lanturn: unsupported network %q", network)
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("lanturn: ListenPacket not supported")
}
