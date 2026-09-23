package option

import O "github.com/sagernet/sing-box/option"

// Hysteria2XOutboundOptions are the stock sing-box hysteria2 outbound options
// plus Lantern's QUIC-censorship evasions. The server side is a stock hysteria2
// inbound; every evasion here is client-only.
type Hysteria2XOutboundOptions struct {
	O.Hysteria2OutboundOptions
	// PreInitialJunk sends one short random UDP datagram to the server on each
	// new UDP flow before the QUIC Initial. The GFW assumes the first datagram of
	// a flow is the Initial; when it can't parse it, it stops inspecting the flow,
	// so the real Initial (and its SNI) goes through uninspected. QUIC servers
	// drop the junk. See GFW Report, "Exposing and Circumventing SNI-based QUIC
	// Censorship of the Great Firewall of China" (USENIX Security 2025), §7.
	PreInitialJunk bool `json:"pre_initial_junk,omitempty"`
}
