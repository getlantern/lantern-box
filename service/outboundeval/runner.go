package outboundeval

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/getlantern/common/backoff"
)

// clientExitCount is how many vantage points a client offers. A client is one
// device on one network, so a sample may never be spread across exits the way a
// proxy-pool runner's can.
const clientExitCount = 1

// retryBaseWait is the default base wait before retrying a cycle a later
// attempt may fix. The backoff jitters it and grows from there, up to the
// configured maximum.
const retryBaseWait = 30 * time.Second

var (
	errUnattestedWindow = errors.New("assignment has a window that could not be attested")
	errAssignmentEnded  = errors.New("assignment ended")
)

func (s *Service) run() {
	defer close(s.done)
	defer s.logger.Info("outbound evaluation runner stopped")
	s.logger.Info("outbound evaluation runner started; first poll in ", time.Duration(s.options.PollInterval))
	if !sleepContext(s.ctx, time.Duration(s.options.PollInterval)) {
		return
	}
	retries := backoff.NewExponentialBackoff(s.retryBase, time.Duration(s.options.MaxRetryBackoff))
	for {
		var err error
		for {
			if err = s.runCycle(); !retryableCycleError(err) {
				break
			}
			s.logger.Debug("outbound evaluation will retry: ", err)
			retries.Wait(s.ctx)
		}
		if s.ctx.Err() != nil {
			return
		}
		retries.Reset()
		interval := time.Duration(s.options.PollInterval)
		if err != nil {
			interval = time.Duration(s.options.NoAssignmentInterval)
			if !errors.Is(err, ErrNoAssignment) {
				s.logger.Warn("outbound evaluation cycle failed: ", err)
			}
		}
		wait := max(interval-time.Since(s.lastAsk), 0)
		s.logger.Trace("next outbound evaluation poll in ", wait)
		if !sleepContext(s.ctx, wait) {
			return
		}
	}
}

func (s *Service) runCycle() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	retrying := s.pendingAssignment != nil
	if s.pendingAssignment == nil {
		s.pendingAssignment = &AssignmentRequest{
			CountryCode:    s.options.CountryCode,
			IdempotencyKey: rand.Text(),
			ExitCount:      clientExitCount,
		}
	}
	s.logger.Trace("requesting outbound evaluation assignment; retry=", retrying)
	s.lastAsk = time.Now()
	assignment, err := s.api.acquire(s.options.Token, *s.pendingAssignment)
	if !retryableCycleError(err) {
		s.pendingAssignment = nil
	}
	if err != nil {
		if errors.Is(err, ErrNoAssignment) {
			s.logger.Debug("no outbound evaluation assignment available; next poll in ", time.Duration(s.options.NoAssignmentInterval))
		}
		return err
	}

	now := s.timeService.TimeFunc()().UTC()
	if err := assignment.validate(now, s.limits); err != nil {
		return fmt.Errorf("validate assignment: %w", err)
	}
	s.logger.Info("outbound evaluation assignment acquired; windows=", len(assignment.Challenges),
		", attempts_per_target_per_window=", assignment.Sample.AttemptsPerWindow,
		", window_budget=", assignment.Sample.windowDuration(),
		", expires_in=", assignment.ExpiresAt.Sub(now))

	started := time.Now()
	report, err := s.measureAssignment(assignment)
	if err != nil {
		return err
	}
	s.logger.Debug("outbound evaluation measurements completed; windows=", len(report.Windows),
		", elapsed=", time.Since(started))
	return s.submitReport(report)
}

// submitReport retries transient submission failures.
func (s *Service) submitReport(report Report) error {
	var err error
	for attempt := 1; attempt <= submitAttempts; attempt++ {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		s.logger.Trace("submitting outbound evaluation report; attempt=", attempt, "/", submitAttempts,
			", windows=", len(report.Windows))
		err = s.api.submit(s.options.Token, report)
		if err == nil {
			s.logger.Info("outbound evaluation report submitted successfully; windows=", len(report.Windows))
			return nil
		}
		if !retryableCycleError(err) {
			s.logger.Debug("outbound evaluation submission failed; retryable=false")
			return err
		}
		if attempt == submitAttempts {
			s.logger.Debug("outbound evaluation submission retries exhausted")
		} else {
			s.logger.Debug("outbound evaluation submission failed; retry in ", s.retryDelay)
		}
		if attempt < submitAttempts && !sleepContext(s.ctx, s.retryDelay) {
			return s.ctx.Err()
		}
	}
	return err
}

func retryableCycleError(err error) bool {
	if err == nil || errors.Is(err, ErrNoAssignment) ||
		errors.Is(err, ErrInvalidContract) || errors.Is(err, errUnattestedWindow) ||
		errors.Is(err, errAssignmentEnded) ||
		errors.Is(err, context.Canceled) {
		return false
	}
	var status apiError
	if errors.As(err, &status) {
		return status.retryable()
	}
	return true
}

// sleepContext waits out delay, or until ctx ends, and reports whether ctx is
// still live.
func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}
