package option

import "github.com/sagernet/sing-box/option"

// ReflexOutboundOptions configures a Reflex outbound proxy.
// In Reflex, the TCP client acts as the TLS server — the proxy server
// initiates the TLS handshake (sends ClientHello). This defeats SNI
// extraction and JA3/JA4 fingerprinting by the censor, since no
// ClientHello ever appears in the client→server direction.
//
// Authentication: the server validates the client's TLS certificate
// fingerprint (SHA-256 of the DER-encoded certificate). No pre-handshake
// bytes are exchanged — the entire auth is within standard TLS.
type ReflexOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// TLS certificate for the client's TLS server role.
	// The SHA-256 fingerprint of this cert serves as authentication —
	// the server validates it during the TLS handshake.
	CertPEM  string `json:"cert_pem,omitempty"`
	KeyPEM   string `json:"key_pem,omitempty"`
	CertPath string `json:"cert_path,omitempty"`
	KeyPath  string `json:"key_path,omitempty"`

	// ConnectTimeout for the TCP + reversed TLS handshake (default: "15s").
	ConnectTimeout string `json:"connect_timeout,omitempty"`
}

// ReflexInboundOptions configures a Reflex inbound proxy.
// The TCP server acts as the TLS client — it sends ClientHello immediately
// upon accepting a connection, then validates the peer's certificate.
type ReflexInboundOptions struct {
	option.ListenOptions

	// AuthTokens contains the SHA-256 fingerprints (lowercase hex-encoded) of
	// allowed peer certificates. In Reflex, the TCP client acts as TLS server
	// and presents a certificate; the TCP server validates its fingerprint.
	// Only connections with a matching fingerprint are accepted.
	AuthTokens []string `json:"auth_tokens"`

	// ServerName is the SNI sent in the TLS ClientHello.
	// Since the server sends the ClientHello, this is invisible to the censor.
	ServerName string `json:"server_name,omitempty"`

	// SilenceTimeout is the duration the server waits for client silence before
	// sending ClientHello. Legitimate Lantern clients send no application data
	// until the server speaks (that's the entire Reflex protocol). Active
	// probes and misdirected TLS clients speak immediately.
	//
	// If the client sends any bytes before this timeout elapses, the connection
	// is transparently forwarded to MasqueradeUpstream instead of receiving a
	// ClientHello. Probes never see Reflex.
	//
	// Set to "0" or leave empty to disable (probing exposure for Reflex).
	// Typical value: "2s". Jittered by SilenceJitter to avoid timing fingerprint.
	SilenceTimeout string `json:"silence_timeout,omitempty"`

	// SilenceJitter is the half-width of a symmetric random offset applied to
	// SilenceTimeout per connection. The actual wait is uniformly distributed
	// in [SilenceTimeout - SilenceJitter, SilenceTimeout + SilenceJitter), so
	// SilenceJitter must be strictly less than SilenceTimeout (otherwise the
	// minimum wait could reach zero and the silence window would be skipped).
	// Defaults to "600ms" when SilenceTimeout is set.
	SilenceJitter string `json:"silence_jitter,omitempty"`

	// MasqueradeUpstream is the host:port of a real TLS service to which
	// connections that fail the silence test are forwarded. Required when
	// SilenceTimeout is set. Example: "www.example.com:443".
	MasqueradeUpstream string `json:"masquerade_upstream,omitempty"`
}
