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
// HTTP transport used for signaling, the current STUN-server list, a logger
// — are injected from the hosting process (e.g. radiance) via context
// (adapter.ContextWithDirectTransport) at service startup, not via this
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
