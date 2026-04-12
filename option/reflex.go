package option

import "github.com/sagernet/sing-box/option"

// ReflexOutboundOptions configures a Reflex outbound proxy.
// In Reflex, the TCP client acts as the TLS server — the proxy server
// initiates the TLS handshake (sends ClientHello). This defeats SNI
// extraction and JA3/JA4 fingerprinting by the censor, since no
// ClientHello ever appears in the client→server direction.
type ReflexOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// AuthToken is a pre-shared 8-byte token (hex-encoded, 16 chars) sent
	// before the TLS handshake to identify this as a Reflex client.
	AuthToken string `json:"auth_token"`

	// TLS certificate for the client's TLS server role.
	// Can be provided inline or as file paths.
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
	CertPath string `json:"cert_path,omitempty"`
	KeyPath  string `json:"key_path,omitempty"`

	// ConnectTimeout for the TCP + reversed TLS handshake (default: "15s").
	ConnectTimeout string `json:"connect_timeout,omitempty"`
}

// ReflexInboundOptions configures a Reflex inbound proxy.
// The TCP server acts as the TLS client — it sends ClientHello after
// receiving the auth token from the TCP client.
type ReflexInboundOptions struct {
	option.ListenOptions

	// AuthTokens is the set of valid pre-shared tokens (hex-encoded).
	// Connections with invalid tokens are closed.
	AuthTokens []string `json:"auth_tokens"`

	// ServerName is the SNI sent in the TLS ClientHello.
	// Since the server sends the ClientHello, this is invisible to the censor.
	ServerName string `json:"server_name,omitempty"`
}
