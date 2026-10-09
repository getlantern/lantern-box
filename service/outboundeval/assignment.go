package outboundeval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	A "github.com/sagernet/sing-box/adapter"
)

var errEmptyAttestation = errors.New("attestation carried no token")

const (
	attestAttempts        = 3
	submitAttempts        = 3
	defaultRetryDelay     = 2 * time.Second
	defaultMeasurementURL = "https://www.wikipedia.org/"
)

// runAssignment fails at the first window it cannot attest, whether
// attestation fails or the assignment ends first, because the server refuses a
// report with an unattested window.
func (s *Service) runAssignment(ctx context.Context, candidate, control A.Outbound, assignment Assignment) (Report, error) {
	report := Report{
		ReportToken:    assignment.ReportToken,
		IdempotencyKey: assignment.ID,
		Windows:        make([]WindowReport, 0, len(assignment.Challenges)),
	}
	for _, challenge := range assignment.Challenges {
		window, err := s.runWindow(ctx, candidate, control, assignment, challenge)
		if err != nil {
			return Report{}, err
		}
		report.Windows = append(report.Windows, window)
	}
	return report, nil
}

func (s *Service) runWindow(
	ctx context.Context,
	candidate, control A.Outbound,
	assignment Assignment,
	challenge WindowChallenge,
) (WindowReport, error) {
	sample := assignment.Sample
	target := assignment.MeasurementURL
	if target == "" {
		target = defaultMeasurementURL
	}
	spacing := max(sample.freshSessionDelay(), time.Duration(s.options.MinFreshSessionDelay))
	s.logger.Trace("outbound evaluation window starting; index=", challenge.WindowIndex,
		", spacing=", spacing, ", budget=", sample.windowDuration())
	// The spacing between windows precedes the window, so it is not charged
	// against the time the window has to measure in.
	if !sleepContext(ctx, spacing) {
		return WindowReport{}, fmt.Errorf("%w before window %d opened: %w",
			errAssignmentEnded, challenge.WindowIndex, ctx.Err())
	}
	windowCtx, cancel := context.WithTimeout(ctx, sample.windowDuration())
	defer cancel()

	attestation, err := s.attestWindow(windowCtx, challenge)
	if err != nil {
		if ctx.Err() != nil {
			return WindowReport{}, fmt.Errorf("%w while attesting window %d: %w",
				errAssignmentEnded, challenge.WindowIndex, ctx.Err())
		}
		return WindowReport{}, fmt.Errorf("%w: window %d: %w", errUnattestedWindow, challenge.WindowIndex, err)
	}
	window := WindowReport{
		AttestationToken:  attestation.Token,
		CandidateAttempts: make([]Attempt, 0, sample.AttemptsPerWindow),
		ControlAttempts:   make([]Attempt, 0, sample.AttemptsPerWindow),
	}
	for attempt := range sample.AttemptsPerWindow {
		if windowCtx.Err() != nil {
			break
		}
		candidateAttempt, controlAttempt := s.measurePair(windowCtx, candidate, control, target)
		for _, result := range []struct {
			role    string
			attempt Attempt
		}{{"candidate", candidateAttempt}, {"control", controlAttempt}} {
			s.logger.Trace("outbound evaluation attempt; window=", challenge.WindowIndex,
				", target=", result.role, ", attempt=", attempt+1,
				", reachable=", result.attempt.Reachable, ", http_status=", result.attempt.HTTPStatus,
				", failure=", result.attempt.FailureCode, ", bytes=", result.attempt.BytesRead,
				", elapsed_ms=", result.attempt.ElapsedMS)
		}
		window.CandidateAttempts = append(window.CandidateAttempts, candidateAttempt)
		window.ControlAttempts = append(window.ControlAttempts, controlAttempt)
	}
	if missing := int(sample.AttemptsPerWindow) - len(window.CandidateAttempts); missing > 0 {
		s.logger.Debug("outbound evaluation window padded; index=", challenge.WindowIndex,
			", missing_pairs=", missing, ", failure=", failureWindowDeadline)
	}
	window = fillWindow(window, sample, failureWindowDeadline)
	window.ObservedAt = s.timeService.TimeFunc()().UTC()
	s.logger.Trace("outbound evaluation window completed; index=", challenge.WindowIndex,
		", candidate_attempts=", len(window.CandidateAttempts),
		", control_attempts=", len(window.ControlAttempts))
	return window, nil
}

// measurePair measures both eval targets at once. Measured one after the
// other, a candidate that times out would spend the window the control's
// attempts need, and the control's padded failures would turn the
// candidate's failure into an inconclusive window.
func (s *Service) measurePair(
	ctx context.Context,
	candidate, control A.Outbound,
	target string,
) (candidateAttempt, controlAttempt Attempt) {
	var wg sync.WaitGroup
	wg.Go(func() { controlAttempt = s.measure(ctx, control, target) })
	candidateAttempt = s.measure(ctx, candidate, target)
	wg.Wait()
	return candidateAttempt, controlAttempt
}

// attestWindow proves the address this window measures from, retrying a failure
// a later attempt may clear.
func (s *Service) attestWindow(ctx context.Context, challenge WindowChallenge) (Attestation, error) {
	request := AttestationRequest{Challenge: challenge.Challenge}
	var err error
	for attempt := 1; attempt <= attestAttempts; attempt++ {
		s.logger.Trace("attesting outbound evaluation window; index=", challenge.WindowIndex,
			", attempt=", attempt, "/", attestAttempts)
		var attestation Attestation
		attestation, err = s.attest(ctx, request)
		if err == nil && attestation.Token == "" {
			err = errEmptyAttestation
		}
		if err == nil {
			s.logger.Trace("outbound evaluation attestation completed; index=", challenge.WindowIndex)
			return attestation, nil
		}
		var status apiError
		if errors.As(err, &status) && !status.retryable() {
			s.logger.Debug("outbound evaluation attestation rejected; index=", challenge.WindowIndex,
				", http_status=", status.status)
			return Attestation{}, err
		}
		s.logger.Debug("outbound evaluation attestation failed; index=", challenge.WindowIndex,
			", attempts_remaining=", attestAttempts-attempt, ": ", err)
		if attempt < attestAttempts && !sleepContext(ctx, s.retryDelay) {
			return Attestation{}, fmt.Errorf("%w (%w)", ctx.Err(), err)
		}
	}
	return Attestation{}, err
}

// fillWindow pads both eval targets up to the sample's width so the grid
// stays complete, and is a no-op for a window that ran every attempt.
func fillWindow(window WindowReport, sample SampleSpec, code string) WindowReport {
	width := int(sample.AttemptsPerWindow)
	failed := Attempt{FailureCode: code}
	if missing := width - len(window.CandidateAttempts); missing > 0 {
		window.CandidateAttempts = append(window.CandidateAttempts, slices.Repeat([]Attempt{failed}, missing)...)
	}
	if missing := width - len(window.ControlAttempts); missing > 0 {
		window.ControlAttempts = append(window.ControlAttempts, slices.Repeat([]Attempt{failed}, missing)...)
	}
	return window
}
