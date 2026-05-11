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
// Each DialContext call opens a fresh lanturn session — one TURN
// allocation + DTLS handshake + SRTP key set — to a Lantern egress
// process colocated with a coturn instance on a Lantern VPS. Bytes
// flow:
//
//	caller bytes
//	  → SRTP-paced chunks at the chosen MediaProfile cadence
//	  → AES-128-CM-HMAC-SHA1-80 (SRTP)
//	  → DTLS-derived keying material (RFC 5764 §4.2)
//	  → TURN ChannelData wrapping
//	  → Plain UDP/3478 to coturn (TURNS-on-5349 fallback planned)
//
// The egress is the lanturn server listening on the same VPS as
// coturn. It receives client bytes on the UDP path coturn relays into
// it; from the egress's perspective, lanturn looks like a SOCKS-shaped
// proxy and would forward bytes onward to the user's destination once
// destination-forwarding is implemented.
//
// # v0.1 alpha status
//
// **The outbound is not yet operational.** DialContext returns a
// clear error because the destination-forwarding handshake between
// the lanturn client and the egress (planned as SOCKS5 CONNECT over
// the lanturn conn) is not yet implemented in pkg/lanturn — without
// it, dialed bytes would be forwarded to a single hardcoded egress
// destination, not the destination the caller asked for. Failing fast
// avoids silent misrouting.
//
// What's wired up:
//
//   - Option validation (NewOutbound rejects missing required fields)
//   - Network advertisement: TCP-only (no UDP — DialContext on UDP
//     would error)
//   - Type registration in the OutboundRegistry
//
// What's deferred to follow-up PRs (working code lives in the
// cmd/lanturn-phase{2,3,4} spike binaries in the lanturn repo):
//
//   - Per-DialContext session multiplexing via SOCKS5-over-lanturn
//     (same pattern Unbounded uses)
//   - covert-dtls fingerprint randomization (currently pion-default;
//     deploy-blocking for Russia / China per design §4.4 + §11.2)
//   - Session rotation across SessionDuration / IdleGap pattern
//   - TURNS-on-5349 fallback
//   - Multi-profile selection (currently Opus-only)
//   - Recency-weighted fleet selection
package lanturn

import (
	"context"
	"fmt"
	"net"

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

	// TCP-only network advertisement — DialContext on UDP would error
	// and ListenPacket isn't implemented, so don't claim to support
	// UDP at the routing layer.
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(C.TypeLanturn, tag, []string{N.NetworkTCP}, opts.DialerOptions),
		logger:  lg,
		cfg:     cfg,
	}, nil
}

// DialContext returns a clear "not yet operational" error.
//
// v0.1 alpha: the destination-forwarding handshake between the lanturn
// client and the egress (planned as SOCKS5 CONNECT) is not yet
// implemented in pkg/lanturn. Returning a real net.Conn here would
// silently forward bytes to a hardcoded egress destination rather than
// the caller-requested one — a correctness bug. The outbound is
// registered as scaffolding for the follow-up that wires destination
// forwarding through; until then, every dial fails fast with a clear
// error.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	if N.NetworkName(network) != N.NetworkTCP {
		return nil, fmt.Errorf("lanturn: %s not supported (TCP only)", network)
	}

	o.logger.WarnContext(ctx, "lanturn DialContext invoked but outbound is not yet operational",
		"dest", destination.String())
	return nil, fmt.Errorf(
		"lanturn: destination forwarding to %s not yet implemented (v0.1 alpha); "+
			"see https://github.com/getlantern/lantern-box/pull/257 — pkg/lanturn needs the SOCKS5-CONNECT-over-lanturn handshake before this outbound can proxy arbitrary destinations",
		destination.String(),
	)
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("lanturn: ListenPacket not supported")
}
