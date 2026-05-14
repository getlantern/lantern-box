package option

import "github.com/sagernet/sing-box/option"

// MITMDFOutboundOptions configures a MITM-DomainFronting outbound.
//
// On dial, the outbound terminates the user's TLS handshake using a leaf
// certificate minted on the fly by a name-constrained local CA (see
// internal/mitmca), looks up the user's SNI against the configured fronts
// table, and dials the matching front through the embedded sing-box
// dialer. The egress connection is wrapped with uTLS using the configured
// fingerprint preset, the front's SNI, and the user's negotiated ALPN.
// Plaintext is bridged between the two TLS sides.
//
// Security properties — see docs/mitm-df/architecture.md and
// getlantern/engineering#3482 for the full rationale:
//
//   - The CA carries an RFC 5280 NameConstraints extension restricting
//     which DNS subtrees it can sign for. Even if the CA private key is
//     extracted, a cert for chase.com cannot be forged.
//   - The outbound never listens on a TCP port; the user side of every
//     MITM session is an in-memory net.Pipe.
//   - A deny list (DenyDomains) is enforced in the GetCertificate
//     callback before any signature is produced — defense in depth for
//     misrouted or maliciously-influenced flows.
//   - Every mint is recorded to an audit log (AuditLogPath).
//
// The user device must trust the CA out-of-band for the user-side TLS
// handshake to succeed.
type MITMDFOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// CA configures the name-constrained signing CA. On first run with
	// missing files at CertPath/KeyPath, a fresh CA scoped to
	// PermittedDomains is generated and persisted. On subsequent runs the
	// existing CA is loaded and its PermittedDNSDomains is checked against
	// the configured PermittedDomains (loads that don't cover the configured
	// list are refused — downgrade protection).
	CA MITMDFCAOptions `json:"ca"`

	// Fronts maps user SNIs onto fronted destinations. The matcher is a
	// suffix-aware exact match against FrontEntry.Names: an SNI matches an
	// entry if it equals one of Names or has one of Names as a `.foo` DNS
	// suffix. Entries are evaluated in order and the first match wins.
	Fronts []MITMDFFrontEntry `json:"fronts"`

	// DenyDomains are SNIs (exact or `.suffix` match, same semantics as
	// Fronts.Names) that the outbound refuses to MITM regardless of any
	// fronts match. Enforced in the GetCertificate callback before signing.
	// Use for sensitive categories (banking, healthcare, government) that
	// the route layer should never have sent here in the first place.
	DenyDomains []string `json:"deny_domains,omitempty"`

	// AuditLogPath is the path of an append-only JSONL audit log. Every
	// MITM decision (allow, deny, no-match) is recorded with timestamp,
	// SNI, fronted SNI, and decision. Empty disables auditing.
	AuditLogPath string `json:"audit_log_path,omitempty"`

	// Fingerprint selects the uTLS ClientHello preset for the egress
	// handshake. One of: "chrome" (default), "firefox", "safari", "random".
	Fingerprint string `json:"fingerprint,omitempty"`

	// EgressHandshakeTimeout bounds both the user-side tls.Server handshake
	// and the egress uTLS handshake, parseable by time.ParseDuration.
	// Defaults to "10s".
	EgressHandshakeTimeout string `json:"egress_handshake_timeout,omitempty"`
}

// MITMDFCAOptions configures the per-device signing CA. CertPath and
// KeyPath are the persistence locations; PermittedDomains is the
// NameConstraints subtree list cryptographically encoded into the cert.
type MITMDFCAOptions struct {
	CertPath         string   `json:"cert_path"`
	KeyPath          string   `json:"key_path"`
	PermittedDomains []string `json:"permitted_domains"`
	// Validity, in time.ParseDuration form. Defaults to 30 days when empty.
	// Only consulted at CA generation; ignored when reloading an existing CA.
	Validity string `json:"validity,omitempty"`
}

// MITMDFFrontEntry maps a set of user-side SNIs to a single fronted
// destination on egress.
type MITMDFFrontEntry struct {
	// Names is the SNI list this front handles. Matched as exact equality
	// or as a `.suffix` DNS subtree of the user's SNI.
	Names []string `json:"names"`

	// FrontedSNI is the SNI presented to the CDN edge on the egress TLS
	// handshake. Required.
	FrontedSNI string `json:"fronted_sni"`

	// VerifySAN, when non-empty, broadens egress cert validation to accept
	// the peer leaf if it is valid for FrontedSNI OR any of these DNS
	// names. Emulates Xray's verifyPeerCertByName. Use when the CDN serves
	// a shared cert covering many names but the user's destination is one
	// of those secondary names (Vercel / Fastly typical case).
	VerifySAN []string `json:"verify_san,omitempty"`

	// RedirectAddr is an optional "host:port" overriding the egress dial
	// target. If empty, the outbound dials the destination handed to it by
	// the router (the user's intended host as resolved). Typical use:
	// dial the front's real backend (`nextjs.org:443`) while preserving the
	// inner Host header.
	RedirectAddr string `json:"redirect_addr,omitempty"`
}
