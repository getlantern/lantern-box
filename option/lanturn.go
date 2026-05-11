package option

import (
	"github.com/sagernet/sing-box/option"
)

// LanturnOutboundOptions configures a client-side lanturn outbound.
//
// lanturn is a Lantern circumvention transport that mimics WebRTC
// TURN-relayed media flow on plain UDP/3478. Bytes the caller writes
// flow through:
//
//   - SRTP-paced chunks (Opus profile, 50pps) at the chosen
//     MediaProfile's cadence
//   - AES-128-CM-HMAC-SHA1-80 encryption (SRTP)
//   - DTLS-derived keying material (RFC 5764 §4.2)
//   - TURN ChannelData wrapping (RFC 8656 §12)
//   - Plain UDP/3478 to a coturn endpoint from CoturnEndpoints
//
// See [getlantern/lanturn](https://github.com/getlantern/lanturn) and
// the design draft in `circumvention-corpus-private` for the full
// protocol design.
//
// # v0.1 alpha note
//
// Only the fields below are honored by the outbound today. Production
// fields planned for later (fingerprint_mode, session_duration_secs,
// idle_gap_*, udp_timeout_ms, prefer_transport) are intentionally
// absent until the upstream pkg/lanturn API supports them — see the
// PHASE-5-TODO markers in github.com/getlantern/lanturn/pkg/lanturn.
//
// Runtime-only dependencies (Credential generation, fleet selection,
// logger) are currently hard-coded in the outbound's NewOutbound: the
// Credential is derived from LanturnAuthSecret, the fleet is iterated
// in order, and the logger is the sing-box ContextLogger. A follow-up
// will plumb these in via context (matching Unbounded's pattern) so a
// hosting process can inject a real config-service Credential callback
// and a recency-weighted FleetSelector.
type LanturnOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// CoturnEndpoints is the fleet of coturn instances the client may
	// allocate from. Production target is 20-50 active endpoints per
	// region (lanturn design §6.2). Each entry has both UDP/3478 +
	// TURNS TCP/5349 addresses (the TURNS addr is parsed but the
	// TURNS-on-5349 fallback path is not wired through in v0.1 — see
	// pkg/lanturn PHASE-5-TODO).
	CoturnEndpoints []LanturnCoturnEndpoint `json:"coturn_endpoints,omitempty"`

	// PeerAddr is the egress's UDP address on the same VPS as coturn
	// (per lanturn design §4.3). Coturn's relay forwards client
	// traffic to this address, and the egress runs lanturn's
	// server-side stack to receive it.
	PeerAddr string `json:"peer_addr"`

	// Profile selects the SRTP-layer media shape. v0.1 only honors
	// "opus" (the upstream MVP only implements that profile). Other
	// values are accepted but silently fall back to Opus until
	// pkg/lanturn fills in vp8 / vp9 / screen-share.
	Profile string `json:"profile,omitempty"`

	// LanturnAuthSecret is the static-auth-secret shared with the
	// coturn instance for OAUTH-shaped credential generation
	// (use-auth-secret pattern). Production: per-endpoint rotated;
	// distributed via Lantern config service rather than this static
	// field. Field is here for v0.1 simple-deploy compatibility.
	LanturnAuthSecret string `json:"lanturn_auth_secret,omitempty"`
}

// LanturnCoturnEndpoint identifies one coturn instance in the fleet.
type LanturnCoturnEndpoint struct {
	UDPAddr    string `json:"udp_addr"`              // host:port plain TURN UDP/3478
	TLSAddr    string `json:"tls_addr,omitempty"`    // host:port TURNS TCP/5349 (optional; fallback not yet wired through)
	ServerName string `json:"server_name,omitempty"` // TLS SNI / cert verify
}

// LanturnInboundOptions configures the egress-side lanturn listener.
// The egress is normally a separate Go binary colocated with coturn
// on a Lantern VPS; for sing-box-style integrated deployment the
// inbound runs in the same lantern-box process.
type LanturnInboundOptions struct {
	option.ListenOptions
	// (no additional fields for v0.1; egress listens on ListenOptions
	// and accepts whatever lanturn clients send)
}
