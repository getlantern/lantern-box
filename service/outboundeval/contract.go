package outboundeval

import (
	"errors"
	"fmt"
	"time"
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

// Assignment is one bounded measurement the server hands out. It names the
// resource to fetch and the sample to take, and nothing about what is under
// test, so a runner cannot attribute its own results.
type Assignment struct {
	ID string `json:"assignment_id"`
	// ReportToken authorizes exactly one report and is the only thing tying
	// that report back to this assignment.
	ReportToken string `json:"report_token"`
	// MeasurementURL is fetched identically by both arms.
	MeasurementURL string            `json:"measurement_url"`
	Sample         SampleSpec        `json:"sample_spec"`
	Challenges     []WindowChallenge `json:"windows"`
	ExpiresAt      time.Time         `json:"expires_at"`
}

// SampleSpec is the grid of measurements one assignment asks for. A report is
// accepted only if it fills the grid exactly.
type SampleSpec struct {
	WindowsPerExit        uint32 `json:"windows_per_exit"`
	AttemptsPerWindow     uint32 `json:"attempts_per_window"`
	WindowDurationSeconds uint32 `json:"window_duration_seconds"`
	// FreshSessionDelayMS is waited before a window opens, spacing windows so
	// they do not share a network moment.
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

// Report is one assignment's complete result. Partial reports are not a thing:
// a window that could not be measured is reported as failed attempts.
type Report struct {
	ReportToken string `json:"report_token"`
	// IdempotencyKey is stable across resubmissions of the same report.
	IdempotencyKey string         `json:"idempotency_key"`
	Windows        []WindowReport `json:"windows"`
}

// WindowReport is one window's paired arms.
type WindowReport struct {
	// AttestationToken identifies the assignment's exit and window; it is empty if attestation failed.
	AttestationToken  string    `json:"exit_attestation_token"`
	CandidateAttempts []Attempt `json:"candidate_attempts"`
	ControlAttempts   []Attempt `json:"control_attempts"`
	ObservedAt        time.Time `json:"observed_at"`
}

// Attempt is one fetch of the measurement URL through one arm.
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

// validate reports whether an assignment can be measured within the local
// bounds.
func (a Assignment) validate(now time.Time, limits bounds) error {
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

// maxBytes is the most the whole grid can transfer, both arms included, when
// every fetch reads its full allowance.
func (s SampleSpec) maxBytes(perResponse int64) int64 {
	return int64(s.WindowsPerExit) * int64(s.AttemptsPerWindow) * 2 * perResponse
}

func (s SampleSpec) windowDuration() time.Duration {
	return time.Duration(s.WindowDurationSeconds) * time.Second
}

func (s SampleSpec) freshSessionDelay() time.Duration {
	return time.Duration(s.FreshSessionDelayMS) * time.Millisecond
}
