package option

import "github.com/sagernet/sing/common/json/badoption"

// BanditProbeOutboundOptions configures the server-side responder for bandit
// callback probes. Route a client's plain-HTTP callback request to this
// outbound and the proxy answers it itself, judges delivery from its own TCP
// state, and forwards the callback to the API. Zero values fall back to the
// defaults noted on each field.
type BanditProbeOutboundOptions struct {
	// CallbackURL is where the proxy sends the callback once it has a verdict,
	// e.g. "https://api.iantem.io/v1/bandit/callback". The client request's
	// query parameters are carried over onto it.
	CallbackURL string `json:"callback_url"`

	// BodySize is the number of random bytes sent back to the client. It has to
	// exceed the point at which censors freeze a flow (default: 65536).
	BodySize int `json:"body_size,omitempty"`

	// StallTimeout is how long acknowledgements may stop advancing, with data
	// still unacknowledged, before the probe is judged stalled (default: 2.5s).
	StallTimeout badoption.Duration `json:"stall_timeout,omitempty"`

	// MaxWait bounds the whole verdict so the callback always reaches the API
	// before its reaper expires the probe (default: 10s).
	MaxWait badoption.Duration `json:"max_wait,omitempty"`

	// ReportStalled sends the callback with verdict=stalled instead of dropping
	// it. Enable it only when the API treats verdict=stalled as a failure;
	// otherwise a stalled probe is counted as a success, whereas a dropped
	// callback is counted as a failure.
	ReportStalled bool `json:"report_stalled,omitempty"`
}
