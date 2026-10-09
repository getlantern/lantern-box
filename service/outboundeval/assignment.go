package outboundeval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	A "github.com/sagernet/sing-box/adapter"
)

const (
	attestAttempts        = 3
	submitAttempts        = 3
	defaultRetryDelay     = 2 * time.Second
	defaultMeasurementURL = "https://www.wikipedia.org/"
)

// runAssignment pads unmeasured attempts on both eval targets to fill the
// assignment's grid.
func (s *Service) runAssignment(ctx context.Context, candidate, control A.Outbound, assignment Assignment) Report {
	report := Report{
		ReportToken:    assignment.ReportToken,
		IdempotencyKey: assignment.ID,
		Windows:        make([]WindowReport, 0, len(assignment.Challenges)),
	}
	for _, challenge := range assignment.Challenges {
		report.Windows = append(report.Windows, s.runWindow(ctx, candidate, control, assignment, challenge))
	}
	return report
}

func (s *Service) runWindow(
	ctx context.Context,
	candidate, control A.Outbound,
	assignment Assignment,
	challenge WindowChallenge,
) (window WindowReport) {
	sample := assignment.Sample
	target := assignment.MeasurementURL
	if target == "" {
		target = defaultMeasurementURL
	}
	window = WindowReport{
		CandidateAttempts: make([]Attempt, 0, sample.AttemptsPerWindow),
		ControlAttempts:   make([]Attempt, 0, sample.AttemptsPerWindow),
	}
	s.logger.Debug("outbound evaluation window starting; index=", challenge.WindowIndex,
		", spacing=", sample.freshSessionDelay(), ", budget=", sample.windowDuration())
	// The result is named so every way out of the window carries the instant
	// it stopped observing.
	defer func() {
		window.ObservedAt = s.timeService.TimeFunc()().UTC()
		s.logger.Debug("outbound evaluation window completed; index=", challenge.WindowIndex,
			", attested=", window.AttestationToken != "",
			", candidate_attempts=", len(window.CandidateAttempts),
			", control_attempts=", len(window.ControlAttempts))
	}()
	// The spacing between windows precedes the window, so it is not charged
	// against the time the window has to measure in.
	if !sleepContext(ctx, sample.freshSessionDelay()) {
		return fillWindow(window, sample, failureWindowDeadline)
	}
	windowCtx, cancel := context.WithTimeout(ctx, sample.windowDuration())
	defer cancel()

	attestation, err := s.attestWindow(windowCtx, challenge)
	if err != nil {
		s.logger.Warn("outbound evaluation window ", challenge.WindowIndex, " went unattested: ", err)
		return fillWindow(window, sample, attestationFailureCode(err))
	}
	window.AttestationToken = attestation.Token

	// Alternating which eval target goes first keeps a systematic advantage
	// from accruing to whichever one always warms the path.
	candidateFirst := challenge.WindowIndex%2 == 0
	for attempt := range sample.AttemptsPerWindow {
		if windowCtx.Err() != nil {
			break
		}
		candidateAttempt, controlAttempt := s.measurePair(
			windowCtx, candidate, control, target, candidateFirst)
		// An interrupted pair is inconclusive for both eval targets.
		if windowCtx.Err() != nil && (interrupted(candidateAttempt) || interrupted(controlAttempt)) {
			candidateAttempt = Attempt{FailureCode: failureWindowDeadline}
			controlAttempt = Attempt{FailureCode: failureWindowDeadline}
		}
		for _, result := range []struct {
			role    string
			attempt Attempt
		}{{"candidate", candidateAttempt}, {"control", controlAttempt}} {
			s.logger.Debug("outbound evaluation attempt; window=", challenge.WindowIndex,
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
	return fillWindow(window, sample, failureWindowDeadline)
}

// interrupted reports whether an attempt ended the way a measurement cut short
// does, rather than completing with a verdict of its own.
func interrupted(attempt Attempt) bool {
	return attempt.FailureCode == failureTimeout || attempt.FailureCode == failureCanceled
}

func (s *Service) measurePair(
	ctx context.Context,
	candidate, control A.Outbound,
	target string,
	candidateFirst bool,
) (candidateAttempt, controlAttempt Attempt) {
	if candidateFirst {
		candidateAttempt = s.measure(ctx, candidate, target)
		controlAttempt = s.measure(ctx, control, target)
	} else {
		controlAttempt = s.measure(ctx, control, target)
		candidateAttempt = s.measure(ctx, candidate, target)
	}
	return candidateAttempt, controlAttempt
}

// attestWindow proves the address this window measures from, retrying a failure
// a later attempt may clear.
func (s *Service) attestWindow(ctx context.Context, challenge WindowChallenge) (Attestation, error) {
	request := AttestationRequest{Challenge: challenge.Challenge}
	var err error
	for attempt := 1; attempt <= attestAttempts; attempt++ {
		s.logger.Debug("attesting outbound evaluation window; index=", challenge.WindowIndex,
			", attempt=", attempt, "/", attestAttempts)
		var attestation Attestation
		attestation, err = s.attest(ctx, request)
		if err == nil {
			s.logger.Debug("outbound evaluation attestation completed; index=", challenge.WindowIndex,
				", token_present=", attestation.Token != "")
			return attestation, nil
		}
		var status apiError
		if errors.As(err, &status) && !status.retryable() {
			s.logger.Debug("outbound evaluation attestation rejected; index=", challenge.WindowIndex,
				", http_status=", status.status)
			return Attestation{}, err
		}
		s.logger.Debug("outbound evaluation attestation failed; index=", challenge.WindowIndex,
			", failure=", attestationFailureCode(err), ", attempts_remaining=", attestAttempts-attempt)
		// Carrying the context error keeps the window's report on the deadline
		// rather than the attestation failure that was about to be retried.
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
