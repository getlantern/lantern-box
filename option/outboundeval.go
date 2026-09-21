package option

import "github.com/sagernet/sing/common/json/badoption"

// OutboundEvalServiceOptions configures the outboundeval service. The three
// endpoint URLs and OutboundTag are required; every other zero value falls
// back to the default documented on the field.
type OutboundEvalServiceOptions struct {
	// AcquireURL, AttestURL and SubmitURL are the control API endpoints, each a
	// complete URL.
	AcquireURL string `json:"acquire_url"`
	AttestURL  string `json:"attest_url"`
	SubmitURL  string `json:"submit_url"`

	// Token authenticates assignment acquisition and report submission.
	// While it is empty the service idles without measuring anything, so an
	// embedder that mints tokens at runtime may leave it unset.
	Token string `json:"token,omitempty"`

	// CountryCode is the market under evaluation, which the server expects as
	// an ISO-3166 alpha-2 code.
	CountryCode string `json:"country_code"`

	// OutboundTag is the outbound measured as the candidate arm. It is resolved
	// again before every cycle, so an embedder may replace the outbound it names
	// while the service runs.
	OutboundTag string `json:"outbound_tag"`

	// ControlOutboundTag is the outbound measured as the control arm and used for
	// every control API call. Exit attestation is attributed to the address it
	// arrives from, so this outbound must egress on the physical interface.
	// Default "direct", which the configuration has to declare: sing-box
	// creates an implicit direct outbound only for a configuration that
	// declares no outbounds at all.
	ControlOutboundTag string `json:"control_outbound_tag,omitempty"`

	// PollInterval is the wait before the first cycle and after a completed
	// assignment. Configuration changes interrupt the wait. Default 5m.
	PollInterval badoption.Duration `json:"poll_interval,omitempty"`

	// NoAssignmentInterval is the wait after the server has nothing to measure,
	// and after it refuses a request in a way a retry cannot fix. Default 5m.
	NoAssignmentInterval badoption.Duration `json:"no_assignment_interval,omitempty"`

	// MaxRetryBackoff caps the backoff applied to retryable control
	// API failures. Default 10m.
	MaxRetryBackoff badoption.Duration `json:"max_retry_backoff,omitempty"`

	// NTPServer is queried for the time an assignment's expiry and observations
	// are judged against. Ignored when the box already runs a time service.
	// Default time.apple.com:123.
	NTPServer string `json:"ntp_server,omitempty"`

	// RequestTimeout bounds each individual HTTP request the service makes.
	// Default 30s.
	RequestTimeout badoption.Duration `json:"request_timeout,omitempty"`

	// MaxResponseBytes caps how much of a measurement response is read.
	// Throughput is reported over the bytes actually read. Default 1 MiB.
	MaxResponseBytes int64 `json:"max_response_bytes,omitempty"`

	// MaxAssignmentBytes caps the response bodies one assignment may read at
	// worst, over every window and both arms; protocol overhead and transport
	// read-ahead are outside it, so it bounds rather than accounts for network
	// use. The candidate arm is proxied, so this is the user's data the service
	// spends, and an assignment whose grid could exceed it is refused rather
	// than measured. Default 32 MiB.
	MaxAssignmentBytes int64 `json:"max_assignment_bytes,omitempty"`

	// MaxWindows and MaxAttemptsPerWindow bound the sample an assignment may ask
	// for. An assignment exceeding either is refused without being measured.
	// Defaults 8 and 8.
	MaxWindows           uint32 `json:"max_windows,omitempty"`
	MaxAttemptsPerWindow uint32 `json:"max_attempts_per_window,omitempty"`
}
