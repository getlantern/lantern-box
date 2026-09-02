package option

import "github.com/sagernet/sing-box/option"

// TwiddleInboundOptions configures the twiddle egress.
//
// twiddle answers a harvested Chrome ClientHello with a synthesised ServerHello
// and then carries an AEAD tunnel framed as TLS application_data. It cannot
// complete a genuine TLS handshake, so MasqueradeUpstream is load-bearing rather
// than optional: every connection that fails to authenticate is forwarded there
// verbatim, and that forwarding is the whole of its probe resistance.
type TwiddleInboundOptions struct {
	option.ListenOptions

	// TicketKey is the server's long-term ticket-encryption key, 32 bytes
	// hex-encoded. Clients never hold it; they hold only tickets it issued.
	TicketKey string `json:"ticket_key"`

	// MasqueradeUpstream is the real cover site unauthenticated connections are
	// forwarded to, as host:port. Without it, an active prober gets a
	// distinguishing reply and the transport is trivially confirmed.
	MasqueradeUpstream string `json:"masquerade_upstream"`

	// TicketMaxAge bounds how long an issued ticket stays valid, as a Go
	// duration. Empty disables the check.
	TicketMaxAge string `json:"ticket_max_age,omitempty"`

	// TicketLen is the ticket size this egress issues. It should match what the
	// impersonated cover identity's real server issues, because the ticket
	// travels inside pre_shared_key and so sets the client's ClientHello size:
	// 176 bytes yields a 1711-byte hello, 230 yields 1761, 256 yields 1806.
	TicketLen int `json:"ticket_len,omitempty"`

	// PSKFirst places pre_shared_key before the other ServerHello extensions.
	// Real servers differ but are each consistent -- google and cloudflare put it
	// first, microsoft and amazon last -- so this should be stable per identity.
	PSKFirst bool `json:"psk_first,omitempty"`
}

// TwiddleOutboundOptions configures the twiddle client.
type TwiddleOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// Ticket is the provisioned ticket, base64. Replaced automatically by the
	// one the egress issues in each connection's flight.
	Ticket string `json:"ticket"`

	// PSK is the pre-shared key paired with Ticket, 32 bytes hex-encoded.
	PSK string `json:"psk"`

	// CoverSNI is the domain this egress masquerades as. It should agree with
	// the egress's masquerade_upstream, or the SNI names one site while probes
	// are answered by another.
	CoverSNI string `json:"cover_sni"`

	// HelloPool carries a pool of harvested ClientHellos inline: one hex-encoded
	// record per line. This is the config-delivered tier -- it beats the pool
	// compiled into the twiddle module, which is a snapshot of one Chrome version
	// and ages into a positive signal, but it loses to HelloPoolDevicePath.
	HelloPool string `json:"hello_pool,omitempty"`

	// HelloPoolPath is a config-written pool file, in the same form as
	// HelloPool. It ranks in the same tier and takes precedence within it, so a
	// config fetcher may deliver hellos either way.
	HelloPoolPath string `json:"hello_pool_path,omitempty"`

	// HelloPoolDevicePath is a pool tapped from this device's OWN outbound TLS
	// traffic, and it outranks both of the above.
	//
	// It is the only source that cannot go stale: by construction it holds what
	// the browser installed on this device emits right now, with this device's
	// Chrome version and field-trial state. Every other source is somebody
	// else's snapshot -- the twiddle module's built-in pool already reproduces no
	// Chrome that exists, having been captured before Chrome dropped
	// server_padding (0x12e0) and added 0xca34.
	//
	// Whatever writes this file MUST pass each hello through twiddle.Sanitize
	// first. A tapped hello names a site the user actually visited, and a
	// resumption hello also carries that site's session ticket.
	HelloPoolDevicePath string `json:"hello_pool_device_path,omitempty"`

	ConnectTimeout string `json:"connect_timeout,omitempty"`
}
