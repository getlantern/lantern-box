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
	errNoToken             = errors.New("idle until a token is supplied")
	errOutboundUnavailable = errors.New("outbound under test is unavailable")
	errUnattestedReport    = errors.New("report contains an unattested window")
)

type pendingAssignment struct {
	token   string
	request AssignmentRequest
}

func (s *Service) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Duration(s.options.PollInterval))
	defer ticker.Stop()
	retries := backoff.NewExponentialBackoff(s.retryBase, time.Duration(s.options.MaxRetryBackoff))
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
		var err error
		for {
			if err = s.runCycle(s.ctx); !retryableCycleError(err) {
				break
			}
			s.logger.Debug("outbound evaluation will retry: ", err)
			retries.WaitOn(s.ctx, s.wake)
		}
		if s.ctx.Err() != nil {
			return
		}
		retries.Reset()
		interval := time.Duration(s.options.PollInterval)
		if err != nil {
			interval = time.Duration(s.options.NoAssignmentInterval)
			if errors.Is(err, ErrNoAssignment) || errors.Is(err, errNoToken) {
				s.logger.Debug("outbound evaluation: ", err)
			} else {
				s.logger.Warn("outbound evaluation refused: ", err)
			}
		}
		ticker.Reset(interval)
	}
}

func (s *Service) runCycle(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config := s.config.Load()
	if config.Token == "" {
		s.pendingAssignment = nil
		return errNoToken
	}
	candidate, found := s.outbounds.Outbound(config.OutboundTag)
	if !found {
		s.pendingAssignment = nil
		return fmt.Errorf("%w: %q", errOutboundUnavailable, config.OutboundTag)
	}

	if s.pendingAssignment == nil || s.pendingAssignment.token != config.Token ||
		s.pendingAssignment.request.CountryCode != config.CountryCode {
		s.pendingAssignment = &pendingAssignment{
			token: config.Token,
			request: AssignmentRequest{
				CountryCode:    config.CountryCode,
				IdempotencyKey: rand.Text(),
				ExitCount:      clientExitCount,
			},
		}
	}
	assignment, err := s.api.acquire(ctx, config.Token, s.pendingAssignment.request)
	if !retryableCycleError(err) {
		s.pendingAssignment = nil
	}
	if err != nil {
		return err
	}

	now := s.timeService.TimeFunc()().UTC()
	if err := assignment.validate(now, s.limits); err != nil {
		return fmt.Errorf("validate assignment: %w", err)
	}

	gridCtx, cancel := context.WithTimeout(ctx, assignment.ExpiresAt.Sub(now))
	defer cancel()
	report := s.runAssignment(gridCtx, candidate, assignment)
	return s.submitReport(ctx, config.Token, report)
}

// submitReport refuses reports with unattested windows and retries transient submission failures.
func (s *Service) submitReport(ctx context.Context, token string, report Report) error {
	for i, window := range report.Windows {
		if window.AttestationToken == "" {
			return fmt.Errorf("%w: window %d", errUnattestedReport, i)
		}
	}
	var err error
	for attempt := 1; attempt <= submitAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = s.api.submit(ctx, token, report)
		if !retryableCycleError(err) {
			return err
		}
		if attempt < submitAttempts && !sleepContext(ctx, s.retryDelay) {
			return ctx.Err()
		}
	}
	return err
}

func retryableCycleError(err error) bool {
	if err == nil || errors.Is(err, ErrNoAssignment) || errors.Is(err, errNoToken) ||
		errors.Is(err, errOutboundUnavailable) || errors.Is(err, ErrInvalidContract) ||
		errors.Is(err, errUnattestedReport) ||
		errors.Is(err, context.Canceled) {
		return false
	}
	var status apiError
	if errors.As(err, &status) {
		return status.retryable()
	}
	return true
}

// sleepContext reports false when ctx ended before the delay elapsed.
func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}
