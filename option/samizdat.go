package option

import "github.com/sagernet/sing-box/option"

// SamizdatOutboundOptions configures a Samizdat outbound proxy.
type SamizdatOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// Authentication
	PublicKey string `json:"public_key"`           // server X25519 public key (hex, 64 chars)
	ShortID   string `json:"short_id"`             // pre-shared 8-byte identifier (hex, 16 chars)

	// TLS fingerprint
	ServerName  string `json:"server_name,omitempty"`  // cover site SNI (e.g. "ok.ru")
	Fingerprint string `json:"fingerprint,omitempty"`  // "chrome" (default), "firefox", "safari"

	// Traffic shaping (enabled by default; set to true to disable)
	DisableJitter bool `json:"disable_jitter,omitempty"` // disable timing jitter
	MaxJitterMs   int  `json:"max_jitter_ms,omitempty"`  // max jitter in ms (default: 30)

	// TCP fragmentation (Geneva-inspired; enabled by default; set to true to disable)
	DisableTCPFragmentation    bool `json:"disable_tcp_fragmentation,omitempty"`    // disable ClientHello fragmentation
	DisableRecordFragmentation bool `json:"disable_record_fragmentation,omitempty"` // disable TLS record fragmentation

	// Connection-start shaping: the first TLS records follow sizes and
	// directions drawn from a model of real TLS (enabled by default; set to
	// true to disable). The server must accept shaping, as samizdat servers
	// do unless their inbound sets disable_shaping.
	DisableShaping bool `json:"disable_shaping,omitempty"`

	// LegacyAuth sends samizdat's version 1 SessionID instead of version 2,
	// for servers older than version 2. Version 1 can be copied into another
	// ClientHello and starts with the short ID in the clear. Servers that
	// old don't accept shaping either, so set disable_shaping too.
	LegacyAuth bool `json:"legacy_auth,omitempty"`

	// CertSPKISHA256 pins the server's certificate: the SHA-256 of the
	// leaf's SubjectPublicKeyInfo, hex (64 chars). With it, a connection the
	// server sends to its cover site (its auth was refused, for instance
	// because the client's clock is more than the server's clock skew off)
	// fails the TLS handshake. Without it, the client completes the cover
	// site's handshake and sends its CONNECT requests, and their
	// destinations, to the cover site. Optional; empty pins nothing.
	CertSPKISHA256 string `json:"cert_spki_sha256,omitempty"`

	// Connection management
	MaxStreamsPerConn int    `json:"max_streams_per_conn,omitempty"` // max H2 streams per TCP conn (default: 100)
	IdleTimeout      string `json:"idle_timeout,omitempty"`         // close idle connections after (default: "5m")
	ConnectTimeout   string `json:"connect_timeout,omitempty"`      // TCP+TLS connect timeout (default: "15s")

}

// SamizdatInboundOptions configures a Samizdat inbound proxy.
type SamizdatInboundOptions struct {
	option.ListenOptions

	// Authentication
	PrivateKey string   `json:"private_key"`           // server X25519 private key (hex, 64 chars)
	ShortIDs   []string `json:"short_ids"`             // allowed client short IDs (hex, 16 chars each)

	// TLS certificate
	CertPath string `json:"cert_path,omitempty"` // path to TLS certificate PEM file
	KeyPath  string `json:"key_path,omitempty"`  // path to TLS key PEM file
	CertPEM  string `json:"cert_pem,omitempty"`  // inline TLS certificate PEM
	KeyPEM   string `json:"key_pem,omitempty"`   // inline TLS key PEM

	// Masquerade
	MasqueradeDomain      string `json:"masquerade_domain,omitempty"`       // domain to masquerade as
	MasqueradeAddr        string `json:"masquerade_addr,omitempty"`         // IP:port override
	MasqueradeIdleTimeout string `json:"masquerade_idle_timeout,omitempty"` // default: "5m"
	MasqueradeMaxDuration string `json:"masquerade_max_duration,omitempty"` // default: "10m"

	// Limits
	MaxConcurrentStreams int `json:"max_concurrent_streams,omitempty"` // per connection (default: 250)

	// DisableShaping stops the inbound accepting clients that shape their
	// connections' first TLS records, which it does by default. Clients that
	// don't shape work either way; clients that do fail against an inbound
	// with it set.
	DisableShaping bool `json:"disable_shaping,omitempty"`

	// RequireAuthV2 refuses samizdat's version 1 SessionIDs, which can be
	// copied into another ClientHello and carry the short ID in the clear.
	// Set it once every client of the inbound sends version 2: clients from
	// before version 2, and outbounds with legacy_auth, then fail.
	RequireAuthV2 bool `json:"require_auth_v2,omitempty"`

	// ReplayWindow is how long the inbound remembers an authenticated
	// SessionID and refuses it again, as a duration (default: "24h"); each is
	// remembered for between it and twice it. Negative turns replay
	// protection off. It must be at least twice max_clock_skew.
	ReplayWindow string `json:"replay_window,omitempty"`

	// MaxClockSkew is how far a version 2 SessionID's time may be from the
	// inbound's, as a duration (default: "12h", or half of replay_window if
	// that is less). A client whose clock is further off is sent to the
	// masquerade.
	MaxClockSkew string `json:"max_clock_skew,omitempty"`
}
