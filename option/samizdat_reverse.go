package option

import "github.com/sagernet/sing-box/option"

// SamizdatPortalInboundOptions configures the inviter's portal inbound that
// accepts the samizdat sessions helpers' bridges dial in.
type SamizdatPortalInboundOptions struct {
	MasqueradeDomain string `json:"masquerade_domain,omitempty"`
	MasqueradeAddr   string `json:"masquerade_addr,omitempty"`
}

// SamizdatReverseOutboundOptions configures an outbound that dials through a
// session the portal inbound has accepted.
type SamizdatReverseOutboundOptions struct {
	Portal string `json:"portal"` // the portal inbound's tag
}

// SamizdatBridgeInboundOptions configures the helper-side bridge that dials
// the inviter's portal and serves the streams it receives.
type SamizdatBridgeInboundOptions struct {
	option.DialerOptions
	Addrs          []string `json:"addrs"`            // "ip:port", at most one IPv4 and one IPv6
	PublicKey      string   `json:"public_key"`       // hex, 32 bytes
	ShortID        string   `json:"short_id"`         // hex, 8 bytes
	ServerName     string   `json:"server_name"`      // cover domain
	CertSPKISHA256 string   `json:"cert_spki_sha256"` // hex, 32 bytes
	Fingerprint    string   `json:"fingerprint,omitempty"`
	// LegacyAuth sends samizdat's version 1 SessionID, for a portal older
	// than version 2 auth.
	LegacyAuth bool `json:"legacy_auth,omitempty"`
}
