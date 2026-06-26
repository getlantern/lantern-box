package option

import "github.com/sagernet/sing-box/option"

// MeekOutboundOptions configures a domain-fronted meek outbound that
// tunnels arbitrary TCP through HTTPS POSTs to a meek server endpoint.
//
// Fronts is the candidate front pool — pairs of (CDN edge IP, outer SNI)
// known to route the inner Host (URL.Host) to the meek server. Radiance
// populates this from the fronted/scanner package's discoveries; without
// at least one front the outbound has nothing to dial.
type MeekOutboundOptions struct {
	option.DialerOptions

	URL    string      `json:"url"`              // meek server URL (e.g. https://meek.dsa.akamai.getiantem.org/)
	Fronts []FrontSpec `json:"fronts"`           // candidate fronts
	Header MeekHeaders `json:"header,omitempty"` // extra HTTP headers per request

	PollIntervalMs int    `json:"poll_interval_ms,omitempty"` // default 100
	MaxBodyBytes   int    `json:"max_body_bytes,omitempty"`   // default 256 KiB (caps request + response bodies per poll)
	SessionIDLen   int    `json:"session_id_len,omitempty"`   // default 16
	ConnectTimeout string `json:"connect_timeout,omitempty"`  // default "15s"
	ReadTimeout    string `json:"read_timeout,omitempty"`     // default "30s"
}

// FrontSpec is one (CDN edge IP, outer SNI) pair to dial. Empty SNI
// means send no ServerName extension (Akamai-style); non-empty SNI is
// sent in the ClientHello (CloudFront-style). VerifyHostname is the
// host expected on the cert chain.
type FrontSpec struct {
	IPAddress      string `json:"ip_address"`
	SNI            string `json:"sni,omitempty"`
	VerifyHostname string `json:"verify_hostname,omitempty"`
}

// MeekHeaders carries fixed-value HTTP headers added to every POST.
type MeekHeaders map[string]string

// MeekInboundOptions configures a meek server inbound: an HTTP meek-v1 endpoint
// (plain HTTP — TLS and CDN fronting are terminated by Caddy/a CDN in front)
// whose tunneled sessions are routed through sing-box. Each session opens with a
// SOCKS5 CONNECT (as the bundled meek outbound sends), which the inbound
// terminates in-process and hands to the router — so no external SOCKS5 proxy
// (microsocks) is required, unlike the standalone cmd/meek-server.
type MeekInboundOptions struct {
	option.ListenOptions

	// MaxBodyBytes caps the request + response body per poll. Default 256 KiB
	// (must match the client's cap, or bodies truncate).
	MaxBodyBytes int `json:"max_body_bytes,omitempty"`
	// ResponseHoldoff is how long the server waits for upstream bytes before
	// responding (possibly empty). Default "50ms".
	ResponseHoldoff string `json:"response_holdoff,omitempty"`
	// SessionIdleTimeout drops a session after this long without a poll; should
	// be >= 2-3x the client's poll interval. Default "5m".
	SessionIdleTimeout string `json:"session_idle_timeout,omitempty"`
	// AuthToken, when set, is the shared secret every request must present in
	// X-Meek-Auth. Strongly recommended for a public/fronted endpoint — without
	// it the server is an open relay. Empty disables the check.
	AuthToken string `json:"auth_token,omitempty"`

	// HTTP server timeouts (empty -> defaults). ReadTimeout/WriteTimeout bound a
	// single poll; IdleTimeout bounds keep-alive reuse between polls.
	ReadTimeout  string `json:"read_timeout,omitempty"`
	WriteTimeout string `json:"write_timeout,omitempty"`
	IdleTimeout  string `json:"idle_timeout,omitempty"`
}
