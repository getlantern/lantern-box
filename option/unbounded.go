package option

import (
	"github.com/sagernet/sing-box/option"
)

// UnboundedOutboundOptions configures a consumer-side Unbounded outbound.
//
// The Unbounded outbound establishes a WebRTC data-channel to a volunteer peer
// (a "producer" running the Unbounded widget in their browser), and tunnels
// QUIC over that channel. See getlantern/broflake for the full protocol.
//
// Only JSON-serializable fields live here. Runtime-only dependencies — the
// HTTP transport used for signaling, the current STUN-server list, a logger,
// the helper pairing tickets — are injected from the hosting process (e.g.
// radiance) via context (adapter.ContextWithDirectTransport,
// adapter.ContextWithRoomTicketSource) at service startup, not via this
// struct. This is a deliberate design choice: sing-box's custom marshaler
// reaches through struct tags and would fail on func or *http.Client fields,
// and polluting the options struct with runtime-injected plumbing would also
// break compatibility with stock sing-box.
//
// Time-valued fields are represented as integer seconds. Zero means "use
// broflake's default"; to disable a timeout rather than default it, pass a
// large value (e.g. the number of seconds in a year). This matches how
// broflake's own options types distinguish unset vs. zero.
type UnboundedOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions

	// TLS verification. The consumer-side outbound acts as the QUIC *server*
	// so the peer presents a client certificate during the handshake. When
	// InsecureDoNotVerifyClientCert is true, we accept any peer cert — useful
	// in dev. In production set EgressCA (a PEM bundle) and EgressServerName
	// (expected DNS SAN / IP SAN) to authenticate the egress peer.
	InsecureDoNotVerifyClientCert bool   `json:"insecure_do_not_verify_client_cert,omitempty"`
	EgressCA                      string `json:"egress_ca,omitempty"`
	EgressServerName              string `json:"egress_server_name,omitempty"`

	// Broflake tunable parameters. Zero means "use broflake default".
	CTableSize  int    `json:"c_table_size,omitempty"`
	PTableSize  int    `json:"p_table_size,omitempty"`
	BusBufferSz int    `json:"bus_buffer_sz,omitempty"`
	Netstated   string `json:"netstated,omitempty"`

	// WebRTC / signaling parameters.
	DiscoverySrv      string   `json:"discovery_srv,omitempty"`
	DiscoveryEndpoint string   `json:"discovery_endpoint,omitempty"`
	// InsecureDoNotVerifyDiscoveryCert skips TLS verification of the
	// signaling server's (freddie's) cert. Only for test/dev against
	// self-signed rigs; production freddie deployments present a real cert
	// and this flag must be false. Ignored when a direct transport is
	// injected on the context (radiance's production path), which carries
	// its own verification policy.
	InsecureDoNotVerifyDiscoveryCert bool `json:"insecure_do_not_verify_discovery_cert,omitempty"`
	GenesisAddr       string   `json:"genesis_addr,omitempty"`
	NATFailTimeout    int      `json:"nat_fail_timeout,omitempty"` // seconds
	// Tuning overrides pion's ICE, DTLS and SCTP defaults (broflake's
	// clientcore.WebRTCTuning), as decimal strings keyed by name: see
	// protocol/unbounded/tuning.go for the keys. It is a map rather than
	// fields so a server can add knobs without breaking older clients, which
	// ignore keys they don't know. Clients that predate the option reject it
	// outright, though, so servers must send it only to clients advertising
	// common.CapabilityUnboundedTuning.
	Tuning map[string]string `json:"tuning,omitempty"`
	STUNBatchSize     int      `json:"stun_batch_size,omitempty"`
	// STUNServers is the full pool. At batch time the outbound samples
	// STUNBatchSize entries at random to avoid a static fingerprint.
	STUNServers       []string `json:"stun_servers,omitempty"`
	Tag               string   `json:"tag,omitempty"`
	Patience          int      `json:"patience,omitempty"`     // seconds
	ErrorBackoff      int      `json:"error_backoff,omitempty"` // seconds
	ConsumerSessionID string   `json:"consumer_session_id,omitempty"`

	// Egress server parameters. Consumer-side desktop peers don't usually
	// reach the egress directly, but we plumb these through for extensibility.
	EgressAddr           string `json:"egress_addr,omitempty"`
	EgressEndpoint       string `json:"egress_endpoint,omitempty"`
	EgressConnectTimeout int    `json:"egress_connect_timeout,omitempty"` // seconds
	EgressErrorBackoff   int    `json:"egress_error_backoff,omitempty"`   // seconds
}

// UnboundedVolunteerOptions configures the volunteer side of Unbounded as a sing-box service: the
// node accepts WebRTC connections from censored users, which freddie matches to it, and relays
// their traffic to the Unbounded egress. A volunteer only dials out, to freddie and the egress, so
// it needs no inbound port.
//
// The fields mirror the volunteer config lantern-cloud gives the apps (common.UnboundedConfig).
// Time-valued fields are integer seconds, and zero means broflake's default, as for
// UnboundedOutboundOptions.
type UnboundedVolunteerOptions struct {
	// DialerOptions apply to signaling with freddie.
	option.DialerOptions

	// DiscoverySrv is freddie's base URL, such as https://freddie.iantem.io. Required.
	DiscoverySrv      string `json:"discovery_srv,omitempty"`
	DiscoveryEndpoint string `json:"discovery_endpoint,omitempty"`
	// InsecureDoNotVerifyDiscoveryCert skips verifying freddie's certificate. Only for test rigs.
	InsecureDoNotVerifyDiscoveryCert bool `json:"insecure_do_not_verify_discovery_cert,omitempty"`

	// EgressAddr is the Unbounded egress, such as wss://unbounded.iantem.io. Required.
	EgressAddr           string `json:"egress_addr,omitempty"`
	EgressEndpoint       string `json:"egress_endpoint,omitempty"`
	EgressConnectTimeout int    `json:"egress_connect_timeout,omitempty"` // seconds
	EgressErrorBackoff   int    `json:"egress_error_backoff,omitempty"`   // seconds

	// MaxPeers is how many censored users the node serves at once. Zero means broflake's default
	// of 5. It sizes both of broflake's worker tables, which a volunteer pairs one to one: each
	// WebRTC slot relays through the egress connection with the same index.
	MaxPeers    int    `json:"max_peers,omitempty"`
	BusBufferSz int    `json:"bus_buffer_sz,omitempty"`
	Netstated   string `json:"netstated,omitempty"`

	// BandwidthLimit caps the node's WebRTC traffic with censored users, in each direction, such
	// as "20mbit", "5Mbps" or "2MB/s": a lowercase b means bits, an uppercase B bytes. It applies
	// to all peers together. Empty or "0" means no limit.
	BandwidthLimit string `json:"bandwidth_limit,omitempty"`

	// STUNServers is the pool the volunteer samples STUN servers from. Required: a volunteer
	// without a working STUN server gets no server reflexive candidate and can't serve.
	STUNServers   []string `json:"stun_servers,omitempty"`
	STUNBatchSize int      `json:"stun_batch_size,omitempty"`
	// Tuning holds the same keys as UnboundedOutboundOptions.Tuning, plus nat_fail_timeout_ms, the
	// lantern-cloud setting unbounded_volunteer_tuning's NAT fail timeout. Unknown or invalid keys
	// are logged and ignored.
	Tuning map[string]string `json:"tuning,omitempty"`
	// NATFailTimeout is how long an attempt waits for ICE, in seconds. Tuning's nat_fail_timeout_ms
	// takes precedence.
	NATFailTimeout int `json:"nat_fail_timeout,omitempty"`
	Patience       int `json:"patience,omitempty"`      // seconds
	ErrorBackoff   int `json:"error_backoff,omitempty"` // seconds

	// TargetCountries, if set, asks freddie to offer the node to consumers from these ISO 3166-1
	// alpha-2 countries first.
	TargetCountries []string `json:"target_countries,omitempty"`

	// StatsInterval is how often the node logs what it served, in seconds. Zero means 60; a
	// negative value turns the line off.
	StatsInterval int `json:"stats_interval,omitempty"`
}
