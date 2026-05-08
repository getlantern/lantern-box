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
//   - Plain UDP/3478 to a coturn endpoint from CoturnEndpoints, with
//     auto-fallback to TURNS-on-TCP/5349 when UDP is unreachable
//
// See [getlantern/lanturn](https://github.com/getlantern/lanturn) and
// [the design draft in circumvention-corpus-private] for the full
// protocol design.
//
// Only JSON-serializable fields live here. Runtime-only dependencies
// (Credential callback, FleetSelector, Logger) come from the hosting
// process via context at service startup, matching the pattern used
// by Unbounded.
type LanturnOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// CoturnEndpoints is the fleet of coturn instances the client may
	// allocate from. Production target is 20-50 active endpoints per
	// region (lanturn design §6.2). Each entry has both UDP/3478 +
	// TURNS TCP/5349 addresses.
	CoturnEndpoints []LanturnCoturnEndpoint `json:"coturn_endpoints,omitempty"`

	// PeerAddr is the egress's UDP address on the same VPS as coturn
	// (per lanturn design §4.3). Coturn's relay forwards client
	// traffic to this address, and the egress runs lanturn's
	// server-side stack to receive it.
	PeerAddr string `json:"peer_addr"`

	// FingerprintMode controls covert-dtls behavior on the inner DTLS
	// handshake. Default = "mimic" (random Chrome ClientHello per
	// session — design §4.4 + cover-dtls catalog §Censor Practice).
	// "randomize" gives per-session diversity at ~85% handshake
	// success rate. "none" uses pion-default (TSPU-blocked since
	// 2026-03; for diagnostic A/B only).
	FingerprintMode string `json:"fingerprint_mode,omitempty"`

	// Profile selects the SRTP-layer media shape: "opus" | "vp8" |
	// "vp9" | "screen" | "random". Default "random".
	Profile string `json:"profile,omitempty"`

	// SessionDuration is the target lifetime of one "call" before
	// rotation. Production target 25-35 minutes (lanturn design §6.1).
	// Default 25 min if zero. In seconds.
	SessionDurationSecs int `json:"session_duration_secs,omitempty"`

	// IdleGapMinSecs / IdleGapMaxSecs bracket the random pause between
	// sessions. Production target 30s-5min. In seconds.
	IdleGapMinSecs int `json:"idle_gap_min_secs,omitempty"`
	IdleGapMaxSecs int `json:"idle_gap_max_secs,omitempty"`

	// UDPTimeoutMs caps how long the client waits on UDP/3478 before
	// falling back to TURNS-on-5349. Default 1500ms.
	UDPTimeoutMs int `json:"udp_timeout_ms,omitempty"`

	// PreferTransport overrides UDP-first selection: "" (auto, default),
	// "udp", or "tls".
	PreferTransport string `json:"prefer_transport,omitempty"`

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
	TLSAddr    string `json:"tls_addr,omitempty"`    // host:port TURNS TCP/5349 (optional)
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
