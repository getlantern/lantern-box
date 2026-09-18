package outboundeval

import (
	"context"
	"errors"
	"time"

	A "github.com/sagernet/sing-box/adapter"
)

const (
	attestAttempts    = 3
	submitAttempts    = 3
	defaultRetryDelay = 2 * time.Second
)

// cycle is what one assignment is measured with: the two arms, and the clock
// its report is stamped against.
type cycle struct {
	candidate A.Outbound
	control   A.Outbound
	clock     serverClock
}

// serverClock advances a server timestamp using local monotonic time. A report
// is accepted against the server's clock, not this device's.
type serverClock struct {
	serverAt   time.Time
	receivedAt time.Time
}

func newServerClock(serverTime time.Time) serverClock {
	return serverClock{serverAt: serverTime, receivedAt: time.Now()}
}

// withServerTime re-anchors to the supplied server timestamp. A zero timestamp
// preserves the current anchor.
func (c serverClock) withServerTime(serverTime time.Time) serverClock {
	if serverTime.IsZero() {
		return c
	}
	return newServerClock(serverTime)
}

func (c serverClock) now() time.Time {
	return c.serverAt.Add(time.Since(c.receivedAt))
}

// runAssignment measures everything the assignment asks for. Its report always
// fills the grid: a window that could not be measured is filled with failed
// attempts on both arms.
func (s *Service) runAssignment(ctx context.Context, c *cycle, assignment Assignment) Report {
	report := Report{
		AssignmentID:   assignment.ID,
		ReportToken:    assignment.ReportToken,
		IdempotencyKey: assignment.ID,
		Windows:        make([]WindowReport, 0, len(assignment.Challenges)),
	}
	for _, challenge := range assignment.Challenges {
		report.Windows = append(report.Windows, s.runWindow(ctx, c, assignment, challenge))
	}
	report.ObservedAt = c.clock.now()
	return report
}

func (s *Service) runWindow(
	ctx context.Context,
	c *cycle,
	assignment Assignment,
	challenge WindowChallenge,
) WindowReport {
	sample := assignment.Sample
	window := WindowReport{ExitIndex: challenge.ExitIndex, WindowIndex: challenge.WindowIndex}
	// The spacing between windows precedes the window, so it is not charged
	// against the time the window has to measure in.
	if !sleepContext(ctx, sample.freshSessionDelay()) {
		return fillWindow(window, sample, failureWindowDeadline)
	}
	windowCtx, cancel := context.WithTimeout(ctx, sample.windowDuration())
	defer cancel()

	attestation, err := s.attestWindow(windowCtx, c, assignment, challenge)
	if err != nil {
		s.logger.Warn("outbound evaluation window ", challenge.WindowIndex, " went unattested: ", err)
		return fillWindow(window, sample, attestationFailureCode(err))
	}
	window.AttestationToken = attestation.Token

	// Alternating which arm goes first keeps a systematic advantage from
	// accruing to whichever arm always warms the path.
	candidateFirst := challenge.WindowIndex%2 == 0
	for range sample.AttemptsPerWindow {
		if windowCtx.Err() != nil {
			break
		}
		candidate, control := s.measurePair(windowCtx, c, assignment.MeasurementURL, candidateFirst)
		// An interrupted pair is inconclusive for both arms.
		if windowCtx.Err() != nil && (interrupted(candidate) || interrupted(control)) {
			candidate = Attempt{FailureCode: failureWindowDeadline}
			control = Attempt{FailureCode: failureWindowDeadline}
		}
		window.CandidateAttempts = append(window.CandidateAttempts, candidate)
		window.ControlAttempts = append(window.ControlAttempts, control)
	}
	return fillWindow(window, sample, failureWindowDeadline)
}

// interrupted reports whether an attempt ended the way a measurement cut short
// does, rather than completing with a verdict of its own.
func interrupted(attempt Attempt) bool {
	return attempt.FailureCode == failureTimeout || attempt.FailureCode == failureCanceled
}

func (s *Service) measurePair(
	ctx context.Context,
	c *cycle,
	target string,
	candidateFirst bool,
) (candidate, control Attempt) {
	if candidateFirst {
		candidate = s.measure(ctx, c.candidate, target)
		control = s.measure(ctx, c.control, target)
	} else {
		control = s.measure(ctx, c.control, target)
		candidate = s.measure(ctx, c.candidate, target)
	}
	return candidate, control
}

// attestWindow proves the address this window measures from, retrying a failure
// a later attempt may clear.
func (s *Service) attestWindow(
	ctx context.Context,
	c *cycle,
	assignment Assignment,
	challenge WindowChallenge,
) (Attestation, error) {
	request := AttestationRequest{
		AssignmentID: assignment.ID,
		ExitIndex:    challenge.ExitIndex,
		WindowIndex:  challenge.WindowIndex,
		Challenge:    challenge.Challenge,
	}
	var err error
	for attempt := 1; attempt <= attestAttempts; attempt++ {
		var attestation Attestation
		attestation, err = s.attest(ctx, s.options.AttestURL, request)
		if err == nil {
			c.clock = c.clock.withServerTime(attestation.ServerTime)
			return attestation, nil
		}
		var status apiError
		if errors.As(err, &status) && !status.retryable() {
			return Attestation{}, err
		}
		if attempt < attestAttempts && !sleepContext(ctx, s.retryDelay) {
			return Attestation{}, err
		}
	}
	return Attestation{}, err
}

// fillWindow pads both arms up to the sample's width so the grid stays
// complete, and is a no-op for a window that ran every attempt.
func fillWindow(window WindowReport, sample SampleSpec, code string) WindowReport {
	width := int(sample.AttemptsPerWindow)
	if missing := width - len(window.CandidateAttempts); missing > 0 {
		window.CandidateAttempts = append(window.CandidateAttempts, failedAttempts(missing, code)...)
	}
	if missing := width - len(window.ControlAttempts); missing > 0 {
		window.ControlAttempts = append(window.ControlAttempts, failedAttempts(missing, code)...)
	}
	return window
}

func attestationFailureCode(err error) string {
	var status apiError
	if errors.As(err, &status) && !status.retryable() {
		return failureAttestationRejected
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return failureWindowDeadline
	}
	return failureAttestation
}
