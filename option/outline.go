package option

import O "github.com/sagernet/sing-box/option"

// OutboundOutlineOptions set the outbound options used by the outline-sdk
// smart dialer. See the smart dialer README for the strategy format:
// https://github.com/OutlineFoundation/outline-sdk/tree/x/v0.3.0/x/smart
// TLS, Fallback and UDP entries are config URLs:
// https://pkg.go.dev/golang.getoutline.org/sdk/x/configurl#hdr-Configuration_Format
type OutboundOutlineOptions struct {
	O.DialerOptions
	O.ServerOptions
	DNSResolvers []DNSEntryConfig `json:"dns,omitempty"`
	TLS          []string         `json:"tls,omitempty"`
	// Fallback lists stream dialer config URLs, such as ss:// or socks5://,
	// tried in order when no DNS/TLS strategy works.
	Fallback []string `json:"fallback,omitempty"`
	// TestTimeout bounds each strategy test. Empty or zero uses the SDK
	// default of five seconds.
	TestTimeout string   `json:"test_timeout"`
	Domains     []string `json:"domains"`
	// UDP enables UDP through the outbound, built from this packet listener
	// config URL over the outbound's own dialer. An empty string is plain UDP;
	// "quicprelude:count=1" sends a decoy QUIC Initial ahead of every datagram
	// that may carry a ClientHello. Unset leaves the outbound TCP-only.
	UDP *string `json:"udp,omitempty"`
	// StrategyCachePath is a file that keeps the strategies found to work, so
	// a restart reuses them instead of searching again. Unset disables it.
	StrategyCachePath string `json:"strategy_cache_path,omitempty"`
	// PerSite gives each site a user reaches over port 443 its own strategy
	// search, which tests that site rather than Domains. Until a site's
	// search succeeds, after one fails, and whenever the site's strategy
	// fails a dial, its dials use the Domains search's strategy. Site
	// searches use only the https and tls resolvers, and are off when there
	// are none. Unset means true.
	PerSite *bool `json:"per_site,omitempty"`
}

// DNSEntryConfig specifies a list of resolvers to test and they can be one of
// the attributes (system, https, tls, udp or tcp)
type DNSEntryConfig struct {
	// System is used for using the system as a resolver, if you want to use it
	// provide an empty object.
	System *struct{} `json:"system,omitempty"`
	// HTTPS use an encrypted DNS over HTTPS (DoH) resolver.
	HTTPS *HTTPSEntryConfig `json:"https,omitempty"`
	// TLS use an encrypted DNS over TLS (DoT) resolver.
	TLS *TLSEntryConfig `json:"tls,omitempty"`
	// UDP use a UDP resolver
	UDP *UDPEntryConfig `json:"udp,omitempty"`
	// TCP use a TCP resolver
	TCP *TCPEntryConfig `json:"tcp,omitempty"`
}

type HTTPSEntryConfig struct {
	// Domain name of the host.
	Name string `json:"name,omitempty"`
	// Host:port. Defaults to Name:443.
	Address string `json:"address,omitempty"`
}

type TLSEntryConfig struct {
	// Domain name of the host.
	Name string `json:"name,omitempty"`
	// Host:port. Defaults to Name:853.
	Address string `json:"address,omitempty"`
}

type UDPEntryConfig struct {
	// Host:port.
	Address string `json:"address,omitempty"`
}

type TCPEntryConfig struct {
	// Host:port.
	Address string `json:"address,omitempty"`
}
