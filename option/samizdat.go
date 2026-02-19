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
	DisablePadding bool   `json:"disable_padding,omitempty"` // disable H2 DATA frame padding
	DisableJitter  bool   `json:"disable_jitter,omitempty"`  // disable timing jitter
	MaxJitterMs    int    `json:"max_jitter_ms,omitempty"`   // max jitter in ms (default: 30)
	PaddingProfile string `json:"padding_profile,omitempty"` // "chrome", "firefox" (default: "chrome")

	// TCP fragmentation (Geneva-inspired; enabled by default; set to true to disable)
	DisableTCPFragmentation    bool `json:"disable_tcp_fragmentation,omitempty"`    // disable ClientHello fragmentation
	DisableRecordFragmentation bool `json:"disable_record_fragmentation,omitempty"` // disable TLS record fragmentation

	// Connection management
	MaxStreamsPerConn int    `json:"max_streams_per_conn,omitempty"` // max H2 streams per TCP conn (default: 100)
	IdleTimeout      string `json:"idle_timeout,omitempty"`         // close idle connections after (default: "5m")
	ConnectTimeout   string `json:"connect_timeout,omitempty"`      // TCP+TLS connect timeout (default: "15s")

	// Russia-specific evasion
	DataThreshold int `json:"data_threshold,omitempty"` // bytes before aggressive padding (default: 14000)
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
}
