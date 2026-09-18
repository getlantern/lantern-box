package outboundeval

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrInvalidContract reports a control API message that does not satisfy the
// runner contract. It is permanent for the message that produced it: resending
// the same message fails the same way.
var ErrInvalidContract = errors.New("invalid outbound evaluation contract")

const (
	maxIdentifierBytes     = 256
	maxTokenBytes          = 4096
	maxURLBytes            = 8192
	maxChallengeBytes      = 512
	maxFailureCodeBytes    = 64
	maxWindowDurationSecs  = 300
	maxFreshSessionDelayMS = 300_000
	maxAssignmentTTL       = 30 * time.Minute
)

var failureCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// AssignmentRequest asks for one measurement assignment. ExitCount is how many
// vantage points the runner offers, which is one for a client.
type AssignmentRequest struct {
	CountryCode string `json:"country_code"`
	ExitCount   uint32 `json:"exit_count"`
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
	Sample         SampleSpec        `json:"sample"`
	Challenges     []WindowChallenge `json:"challenges"`
	ExpiresAt      time.Time         `json:"expires_at"`
	// ServerTime is the server's clock when it issued the assignment. A report
	// is accepted against that clock, so it anchors ObservedAt.
	ServerTime time.Time `json:"server_time"`
}

// SampleSpec is the grid of measurements one assignment asks for. A report is
// accepted only if it fills the grid exactly.
type SampleSpec struct {
	WindowsPerExit        uint32 `json:"windows_per_exit"`
	AttemptsPerWindow     uint32 `json:"attempts_per_window"`
	WindowDurationSeconds uint32 `json:"window_duration_seconds"`
	// FreshSessionDelayMS is waited before a window opens, spacing windows so
	// they do not share a network moment.
	FreshSessionDelayMS uint32 `json:"fresh_session_delay_ms"`
}

// WindowChallenge is the opaque value one window attests with. The server binds
// it to the window it names, so it is usable in no other window.
type WindowChallenge struct {
	ExitIndex   uint32 `json:"exit_index"`
	WindowIndex uint32 `json:"window_index"`
	Challenge   string `json:"challenge"`
}

// AttestationRequest proves which address a window measured from. It must reach
// the server over the physical interface, because the server attributes the
// window to the address the request arrives from.
type AttestationRequest struct {
	AssignmentID string `json:"assignment_id"`
	ExitIndex    uint32 `json:"exit_index"`
	WindowIndex  uint32 `json:"window_index"`
	Challenge    string `json:"challenge"`
}

// Attestation is the server's answer to an AttestationRequest.
type Attestation struct {
	Token      string    `json:"attestation_token"`
	ServerTime time.Time `json:"server_time"`
}

// Report is one assignment's complete result. Partial reports are not a thing:
// a window that could not be measured is reported as failed attempts.
type Report struct {
	AssignmentID string `json:"assignment_id"`
	ReportToken  string `json:"report_token"`
	// IdempotencyKey is stable across resubmissions of the same report.
	IdempotencyKey string         `json:"idempotency_key"`
	ObservedAt     time.Time      `json:"observed_at"`
	Windows        []WindowReport `json:"windows"`
}

// WindowReport is one window's paired arms.
type WindowReport struct {
	ExitIndex   uint32 `json:"exit_index"`
	WindowIndex uint32 `json:"window_index"`
	// AttestationToken is empty when attestation failed, which leaves the
	// window tied to its assignment but attributed to no country or ASN.
	AttestationToken  string    `json:"attestation_token,omitempty"`
	CandidateAttempts []Attempt `json:"candidate_attempts"`
	ControlAttempts   []Attempt `json:"control_attempts"`
}

// Attempt is one fetch of the measurement URL through one arm.
type Attempt struct {
	Reachable  bool `json:"reachable"`
	HTTPStatus int  `json:"http_status,omitempty"`
	// TimeToHeadersMS spans the request up to the response headers.
	TimeToHeadersMS int64 `json:"time_to_headers_ms"`
	// ElapsedMS additionally spans reading the body.
	ElapsedMS                int64   `json:"elapsed_ms"`
	BytesRead                int64   `json:"bytes_read"`
	ThroughputBytesPerSecond float64 `json:"throughput_bytes_per_second"`
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
	if err := boundedIdentifier("assignment id", a.ID, maxIdentifierBytes); err != nil {
		return err
	}
	if err := boundedIdentifier("report token", a.ReportToken, maxTokenBytes); err != nil {
		return err
	}
	if err := validateMeasurementURL(a.MeasurementURL); err != nil {
		return err
	}
	if err := a.Sample.validate(limits); err != nil {
		return err
	}
	if a.ServerTime.IsZero() {
		return fmt.Errorf("%w: assignment carries no server time", ErrInvalidContract)
	}
	if !a.ExpiresAt.After(now) || a.ExpiresAt.After(now.Add(maxAssignmentTTL)) {
		return fmt.Errorf("%w: assignment expiry %s is not within %s of now",
			ErrInvalidContract, a.ExpiresAt, maxAssignmentTTL)
	}
	// A grid that cannot finish before the assignment expires would report its
	// trailing windows as deadline failures, so it is refused unmeasured.
	if needed, remaining := a.Sample.maxDuration(), a.ExpiresAt.Sub(now); needed > remaining {
		return fmt.Errorf("%w: sample needs %s but the assignment expires in %s",
			ErrInvalidContract, needed, remaining)
	}
	if worst := a.Sample.maxBytes(limits.responseBytes); worst > limits.assignmentBytes {
		return fmt.Errorf("%w: sample could transfer %d bytes, local maximum is %d",
			ErrInvalidContract, worst, limits.assignmentBytes)
	}
	return a.validateChallenges()
}

func (a Assignment) validateChallenges() error {
	want := int(a.Sample.WindowsPerExit)
	if len(a.Challenges) != want {
		return fmt.Errorf("%w: assignment has %d challenges, want %d",
			ErrInvalidContract, len(a.Challenges), want)
	}
	seen := make(map[uint32]struct{}, want)
	for _, challenge := range a.Challenges {
		if challenge.ExitIndex != 0 {
			return fmt.Errorf("%w: assignment challenge names exit %d, but a client is one exit",
				ErrInvalidContract, challenge.ExitIndex)
		}
		if challenge.WindowIndex >= a.Sample.WindowsPerExit {
			return fmt.Errorf("%w: assignment challenge names window %d outside the sample",
				ErrInvalidContract, challenge.WindowIndex)
		}
		if challenge.Challenge == "" || len(challenge.Challenge) > maxChallengeBytes {
			return fmt.Errorf("%w: assignment challenge for window %d is empty or oversized",
				ErrInvalidContract, challenge.WindowIndex)
		}
		if _, duplicate := seen[challenge.WindowIndex]; duplicate {
			return fmt.Errorf("%w: assignment repeats a challenge for window %d",
				ErrInvalidContract, challenge.WindowIndex)
		}
		seen[challenge.WindowIndex] = struct{}{}
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

// maxDuration is the whole grid's time at worst, spacing included.
func (s SampleSpec) maxDuration() time.Duration {
	return time.Duration(s.WindowsPerExit) * (s.windowDuration() + s.freshSessionDelay())
}

func (s SampleSpec) windowDuration() time.Duration {
	return time.Duration(s.WindowDurationSeconds) * time.Second
}

func (s SampleSpec) freshSessionDelay() time.Duration {
	return time.Duration(s.FreshSessionDelayMS) * time.Millisecond
}

// validate reports whether the report fills the sample's grid exactly, which is
// what the server requires of it.
func (r Report) validate(sample SampleSpec) error {
	if len(r.Windows) != int(sample.WindowsPerExit) {
		return fmt.Errorf("%w: report has %d windows, want %d",
			ErrInvalidContract, len(r.Windows), sample.WindowsPerExit)
	}
	for _, window := range r.Windows {
		if len(window.CandidateAttempts) != int(sample.AttemptsPerWindow) ||
			len(window.ControlAttempts) != int(sample.AttemptsPerWindow) {
			return fmt.Errorf("%w: report window %d has %d candidate and %d control attempts, want %d of each",
				ErrInvalidContract, window.WindowIndex,
				len(window.CandidateAttempts), len(window.ControlAttempts), sample.AttemptsPerWindow)
		}
		for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
			for _, attempt := range attempts {
				if err := attempt.validate(); err != nil {
					return fmt.Errorf("report window %d: %w", window.WindowIndex, err)
				}
			}
		}
	}
	return nil
}

func (a Attempt) validate() error {
	if a.Reachable == (a.FailureCode != "") {
		return fmt.Errorf("%w: attempt must carry a failure code exactly when unreachable", ErrInvalidContract)
	}
	if a.FailureCode != "" &&
		(len(a.FailureCode) > maxFailureCodeBytes || !failureCodePattern.MatchString(a.FailureCode)) {
		return fmt.Errorf("%w: failure code %q is not a bounded lower snake-case code",
			ErrInvalidContract, a.FailureCode)
	}
	return nil
}

func boundedIdentifier(name, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidContract, name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidContract, name, maxBytes)
	}
	return nil
}

// parseBoundedURL parses raw as an absolute request URI no longer than maxBytes,
// rejecting one that is unparseable, hostless, or carries userinfo. Scheme and
// address policy are the caller's to apply to the returned URL.
func parseBoundedURL(raw string, maxBytes int) (*url.URL, error) {
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return nil, fmt.Errorf("is unusable: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("must have a host and no credentials")
	}
	return parsed, nil
}

func validateMeasurementURL(raw string) error {
	parsed, err := parseBoundedURL(raw, maxURLBytes)
	if err != nil {
		return fmt.Errorf("%w: measurement URL %w", ErrInvalidContract, err)
	}
	// ParseRequestURI folds a fragment into the path instead of populating
	// Fragment, so a literal '#' has to be rejected on its own.
	if parsed.Scheme != "https" || strings.Contains(raw, "#") {
		return fmt.Errorf("%w: measurement URL must be HTTPS without a fragment", ErrInvalidContract)
	}
	// The control arm reaches this destination on the physical interface, so a
	// literal address has to be public. A hostname's resolved address is not
	// checked, which leaves the control API trusted for that much.
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate()) {
		return fmt.Errorf("%w: measurement URL must not name a private or local address",
			ErrInvalidContract)
	}
	return nil
}
