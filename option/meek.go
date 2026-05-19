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

	URL    string      `json:"url"`              // meek server URL (e.g. https://api.iantem.io/meek/)
	Fronts []FrontSpec `json:"fronts"`           // candidate fronts
	Header MeekHeaders `json:"header,omitempty"` // extra HTTP headers per request

	PollIntervalMs int    `json:"poll_interval_ms,omitempty"` // default 100
	MaxBodyBytes   int    `json:"max_body_bytes,omitempty"`   // default 64 KiB
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
