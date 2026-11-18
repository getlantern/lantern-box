package outboundeval

import (
	"context"
	"errors"
	"fmt"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
)

// ErrInvalidContract reports a control API message that does not satisfy the
// runner contract. It is permanent for the message that produced it: resending
// the same message fails the same way.
var ErrInvalidContract = errors.New("invalid outbound evaluation contract")

const (
	maxWindowDurationSecs  = 300
	maxFreshSessionDelayMS = 300_000
)

// AssignmentRequest asks for one measurement assignment. ExitCount is how many
// vantage points the runner offers, which is one for a client.
type AssignmentRequest struct {
	CountryCode string `json:"country_code"`
	// IdempotencyKey identifies one acquisition across retries.
	IdempotencyKey string `json:"idempotency_key"`
	ExitCount      uint32 `json:"exit_count"`
}

// Assignment is one bounded measurement issued by the server.
type Assignment struct {
	ID string `json:"assignment_id"`
	// ReportToken authorizes exactly one report and is the only thing tying
	// that report back to this assignment.
	ReportToken string `json:"report_token"`
	// MeasurementURL is fetched by both eval targets; empty selects https://www.wikipedia.org/.
	MeasurementURL string `json:"measurement_url,omitempty"`
	// Candidate and Control are the pair measured. An assignment lacking
	// either is refused.
	Candidate  *EvaluationTarget `json:"candidate_target,omitempty"`
	Control    *EvaluationTarget `json:"control_target,omitempty"`
	Sample     SampleSpec        `json:"sample_spec"`
	Challenges []WindowChallenge `json:"windows"`
	ExpiresAt  time.Time         `json:"expires_at"`
}

// Eval target types specify which kind of option an eval target carries,
// [option.Outbound] or [option.Endpoint].
const (
	EvaluationTargetOutbound = "outbound"
	EvaluationTargetEndpoint = "endpoint"
)

// EvaluationTarget configures an outbound or endpoint for an assignment.
//
// An EvaluationTarget encodes and decodes only through sing's context JSON;
// the standard library cannot produce or consume the typed sing-box options
// it carries.
type EvaluationTarget struct {
	Type string
	// Options is an option.Outbound for EvaluationTargetOutbound or an
	// option.Endpoint for EvaluationTargetEndpoint.
	Options any
}

type evaluationTargetJSON struct {
	Type    string          `json:"type"`
	Options json.RawMessage `json:"options"`
}

// MarshalJSONContext fails for a Type it does not know or Options that do not
// match it.
func (t *EvaluationTarget) MarshalJSONContext(ctx context.Context) ([]byte, error) {
	var (
		options []byte
		err     error
	)
	switch t.Type {
	case EvaluationTargetOutbound:
		outbound, ok := t.Options.(option.Outbound)
		if !ok {
			return nil, fmt.Errorf("outbound target holds %T", t.Options)
		}
		options, err = outbound.MarshalJSONContext(ctx)
	case EvaluationTargetEndpoint:
		endpoint, ok := t.Options.(option.Endpoint)
		if !ok {
			return nil, fmt.Errorf("endpoint target holds %T", t.Options)
		}
		options, err = endpoint.MarshalJSONContext(ctx)
	default:
		return nil, fmt.Errorf("unknown target type %q", t.Type)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(evaluationTargetJSON{Type: t.Type, Options: options})
}

// UnmarshalJSONContext returns ErrInvalidContract for malformed or unsupported
// eval targets.
func (t *EvaluationTarget) UnmarshalJSONContext(ctx context.Context, content []byte) error {
	var wire evaluationTargetJSON
	if err := json.Unmarshal(content, &wire); err != nil {
		return fmt.Errorf("%w: evaluation target: %w", ErrInvalidContract, err)
	}
	switch wire.Type {
	case EvaluationTargetOutbound:
		var outbound option.Outbound
		if err := outbound.UnmarshalJSONContext(ctx, wire.Options); err != nil {
			return fmt.Errorf("%w: outbound target: %w", ErrInvalidContract, err)
		}
		t.Options = outbound
	case EvaluationTargetEndpoint:
		var endpoint option.Endpoint
		if err := endpoint.UnmarshalJSONContext(ctx, wire.Options); err != nil {
			return fmt.Errorf("%w: endpoint target: %w", ErrInvalidContract, err)
		}
		t.Options = endpoint
	default:
		return fmt.Errorf("%w: target type %q is neither %q nor %q",
			ErrInvalidContract, wire.Type, EvaluationTargetOutbound, EvaluationTargetEndpoint)
	}
	t.Type = wire.Type
	return nil
}

// SampleSpec is the grid of measurements one assignment asks for. A report is
// accepted only if it fills the grid exactly.
type SampleSpec struct {
	WindowsPerExit        uint32 `json:"windows_per_exit"`
	AttemptsPerWindow     uint32 `json:"attempts_per_window"`
	WindowDurationSeconds uint32 `json:"window_duration_seconds"`
	// FreshSessionDelayMS is the spacing the assignment requests before each
	// window, so windows do not share a network moment.
	FreshSessionDelayMS uint32 `json:"fresh_session_delay_milliseconds"`
}

// WindowChallenge is the opaque value one window attests with. The server binds
// it to the window it names, so it is usable in no other window.
type WindowChallenge struct {
	ExitIndex   uint32 `json:"exit_index"`
	WindowIndex uint32 `json:"window_index"`
	Challenge   string `json:"exit_attestation_challenge"`
}

// AttestationRequest proves which address a window measured from. It must reach
// the server over the physical interface, because the server attributes the
// window to the address the request arrives from.
type AttestationRequest struct {
	Challenge string `json:"challenge"`
}

// Attestation is the server's answer to an AttestationRequest.
type Attestation struct {
	Token string `json:"attestation_token"`
}

// Report is one assignment's complete result: every window is attested and
// fills the sample's grid.
type Report struct {
	ReportToken string `json:"report_token"`
	// IdempotencyKey is stable across resubmissions of the same report.
	IdempotencyKey string         `json:"idempotency_key"`
	Windows        []WindowReport `json:"windows"`
}

// WindowReport is one window's attempts through both eval targets.
type WindowReport struct {
	// AttestationToken identifies the assignment's exit and window; it is never empty.
	AttestationToken  string    `json:"exit_attestation_token"`
	CandidateAttempts []Attempt `json:"candidate_attempts"`
	ControlAttempts   []Attempt `json:"control_attempts"`
	ObservedAt        time.Time `json:"observed_at"`
}

// Attempt is one fetch of the measurement URL through one eval target.
type Attempt struct {
	Reachable  bool `json:"reachable"`
	HTTPStatus int  `json:"http_status,omitempty"`
	// TimeToHeadersMS spans the request up to the response headers.
	TimeToHeadersMS float64 `json:"latency_ms"`
	// ElapsedMS additionally spans reading the body.
	ElapsedMS int64 `json:"elapsed_milliseconds"`
	BytesRead int64 `json:"bytes_transferred"`
	// ThroughputBytesPerSecond is truncated toward zero.
	ThroughputBytesPerSecond int64 `json:"throughput_bytes_per_second"`
	// FailureCode is non-empty exactly when Reachable is false.
	FailureCode string `json:"failure_code,omitempty"`
}

// bounds cap what a server may ask one client to do.
type bounds struct {
	windows           uint32
	attemptsPerWindow uint32
	responseBytes     int64
	assignmentBytes   int64
}

// validate refuses an assignment that breaks the runner contract or exceeds
// the local bounds.
func (a Assignment) validate(now time.Time, limits bounds) error {
	if a.ID == "" || a.ReportToken == "" {
		return fmt.Errorf("%w: assignment lacks an ID or report token", ErrInvalidContract)
	}
	if err := a.Sample.validate(limits); err != nil {
		return err
	}
	if !a.ExpiresAt.After(now) {
		return fmt.Errorf("%w: assignment expired at %s", ErrInvalidContract, a.ExpiresAt)
	}
	if worst := a.Sample.maxBytes(limits.responseBytes); worst > limits.assignmentBytes {
		return fmt.Errorf("%w: sample could transfer %d bytes, local maximum is %d",
			ErrInvalidContract, worst, limits.assignmentBytes)
	}
	want := int(a.Sample.WindowsPerExit)
	if len(a.Challenges) != want {
		return fmt.Errorf("%w: assignment has %d challenges, want %d",
			ErrInvalidContract, len(a.Challenges), want)
	}
	seen := make(map[uint32]bool, len(a.Challenges))
	for _, challenge := range a.Challenges {
		switch {
		case challenge.ExitIndex >= clientExitCount || challenge.WindowIndex >= a.Sample.WindowsPerExit:
			return fmt.Errorf("%w: challenge for exit %d window %d is outside the sample",
				ErrInvalidContract, challenge.ExitIndex, challenge.WindowIndex)
		case seen[challenge.WindowIndex]:
			return fmt.Errorf("%w: duplicate challenge for window %d", ErrInvalidContract, challenge.WindowIndex)
		case challenge.Challenge == "":
			return fmt.Errorf("%w: empty challenge for window %d", ErrInvalidContract, challenge.WindowIndex)
		}
		seen[challenge.WindowIndex] = true
	}
	for _, target := range []*EvaluationTarget{a.Candidate, a.Control} {
		if err := target.validate(); err != nil {
			return err
		}
	}
	// The server tags each route's target uniquely, so a shared tag is one
	// route on both sides of the pair.
	if a.Candidate.tag() == a.Control.tag() {
		return fmt.Errorf("%w: candidate and control are both %q", ErrInvalidContract, a.Candidate.tag())
	}
	return nil
}

func (t *EvaluationTarget) tag() string {
	switch options := t.Options.(type) {
	case option.Outbound:
		return options.Tag
	case option.Endpoint:
		return options.Tag
	}
	return ""
}

// nonProxyTypes are the outbound types that do not tunnel to a proxy of
// their own, so a measurement through one is not of a proxy.
var nonProxyTypes = map[string]bool{
	C.TypeDirect: true, C.TypeBlock: true, C.TypeDNS: true,
	C.TypeSelector: true, C.TypeURLTest: true,
	lbC.TypeFallback: true, lbC.TypeMutableSelector: true,
	lbC.TypeMutableURLTest: true, lbC.TypeMutableAutoSelect: true,
	lbC.TypeBanditProbe: true,
}

// validate refuses an eval target that is not a proxy of its own or that
// detours through another outbound, and an endpoint that would listen on the
// device, bring up a system interface, or whose type it cannot check for
// either.
func (t *EvaluationTarget) validate() error {
	if t == nil {
		return fmt.Errorf("%w: assignment lacks an eval target", ErrInvalidContract)
	}
	var (
		kind    string
		options any
	)
	switch target := t.Options.(type) {
	case option.Outbound:
		kind, options = target.Type, target.Options
	case option.Endpoint:
		kind, options = target.Type, target.Options
	default:
		return fmt.Errorf("%w: %s eval target holds %T", ErrInvalidContract, t.Type, t.Options)
	}
	if nonProxyTypes[kind] {
		return fmt.Errorf("%w: eval target type %q is not a proxy", ErrInvalidContract, kind)
	}
	if dialer, ok := options.(option.DialerOptionsWrapper); ok && dialer.TakeDialerOptions().Detour != "" {
		return fmt.Errorf("%w: eval target detours through %q",
			ErrInvalidContract, dialer.TakeDialerOptions().Detour)
	}
	if t.Type != EvaluationTargetEndpoint {
		return nil
	}
	var wireguard *option.WireGuardEndpointOptions
	switch options := options.(type) {
	case *option.WireGuardEndpointOptions:
		wireguard = options
	case *lbO.AmneziaEndpointOptions:
		wireguard = &options.WireGuardEndpointOptions
	default:
		return fmt.Errorf("%w: endpoint type %q is not an accepted eval target",
			ErrInvalidContract, kind)
	}
	switch {
	case wireguard.ListenPort != 0:
		return fmt.Errorf("%w: endpoint eval target listens on port %d",
			ErrInvalidContract, wireguard.ListenPort)
	case wireguard.System:
		return fmt.Errorf("%w: endpoint eval target brings up a system interface", ErrInvalidContract)
	}
	return nil
}

func (s SampleSpec) validate(limits bounds) error {
	switch {
	case s.WindowsPerExit == 0 || s.WindowsPerExit > limits.windows:
		return fmt.Errorf("%w: sample asks for %d windows, local maximum is %d",
			ErrInvalidContract, s.WindowsPerExit, limits.windows)
	case s.AttemptsPerWindow == 0 || s.AttemptsPerWindow > limits.attemptsPerWindow:
		return fmt.Errorf("%w: sample asks for %d attempts per window, local maximum is %d",
			ErrInvalidContract, s.AttemptsPerWindow, limits.attemptsPerWindow)
	case s.WindowDurationSeconds == 0 || s.WindowDurationSeconds > maxWindowDurationSecs:
		return fmt.Errorf("%w: sample window duration %ds is outside 1s..%ds",
			ErrInvalidContract, s.WindowDurationSeconds, maxWindowDurationSecs)
	case s.FreshSessionDelayMS > maxFreshSessionDelayMS:
		return fmt.Errorf("%w: sample fresh-session delay %dms exceeds %dms",
			ErrInvalidContract, s.FreshSessionDelayMS, maxFreshSessionDelayMS)
	}
	return nil
}

// maxBytes is the most the whole grid can transfer, both eval targets
// included, when every fetch reads its full allowance.
func (s SampleSpec) maxBytes(perResponse int64) int64 {
	return int64(s.WindowsPerExit) * int64(s.AttemptsPerWindow) * 2 * perResponse
}

func (s SampleSpec) windowDuration() time.Duration {
	return time.Duration(s.WindowDurationSeconds) * time.Second
}

func (s SampleSpec) freshSessionDelay() time.Duration {
	return time.Duration(s.FreshSessionDelayMS) * time.Millisecond
}
