package outboundeval

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
)

type createdTarget struct {
	tag    string
	remove func(tag string) error
}

func (s *Service) measureAssignment(candidateTag string, assignment Assignment) (report Report, err error) {
	now := s.timeService.TimeFunc()()
	ctx, cancel := context.WithTimeout(s.ctx, assignment.ExpiresAt.Sub(now))
	defer cancel()
	var candidate A.Outbound
	control := s.control
	if assignment.Candidate != nil && assignment.Control != nil {
		s.logger.Debug("using assignment-provided evaluation targets")
		defer func() {
			err = errors.Join(err, s.closeAssignmentOutbounds())
		}()
		candidate, control, err = s.createAssignmentOutbounds(ctx, assignment)
		if err != nil {
			return Report{}, err
		}
	} else {
		s.logger.Debug("using configured evaluation targets")
		var found bool
		candidate, found = s.outbounds.Outbound(candidateTag)
		if !found {
			return Report{}, fmt.Errorf("%w: %q", errOutboundUnavailable, candidateTag)
		}
	}
	return s.runAssignment(ctx, candidate, control, assignment), nil
}

func (s *Service) createAssignmentOutbounds(ctx context.Context, assignment Assignment) (A.Outbound, A.Outbound, error) {
	s.assignmentMu.Lock()
	defer s.assignmentMu.Unlock()
	var targets [2]A.Outbound
	for i, target := range []*EvaluationTarget{assignment.Candidate, assignment.Control} {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Server-provided tags must not replace configured outbounds.
		tag := "outbound-eval-" + rand.Text()
		out, err := s.createTargetLocked(ctx, tag, target)
		if err != nil {
			return nil, nil, fmt.Errorf("create assignment target %d: %w", i, err)
		}
		targets[i] = out
		s.logger.Debug("outbound evaluation target created; index=", i)
	}
	return targets[0], targets[1], nil
}

// createTargetLocked must be called with assignmentMu held. A target is
// recorded for removal as soon as it exists, so one that is created but not
// found is still torn down.
func (s *Service) createTargetLocked(ctx context.Context, tag string, target *EvaluationTarget) (A.Outbound, error) {
	var (
		out   A.Outbound
		found bool
	)
	switch target.Type {
	case EvaluationTargetOutbound:
		options, ok := target.Options.(option.Outbound)
		if !ok {
			return nil, fmt.Errorf("outbound target holds %T", target.Options)
		}
		if err := s.outbounds.Create(ctx, s.router, s.logger, tag, options.Type, options.Options); err != nil {
			return nil, err
		}
		s.assignmentTargets = append(s.assignmentTargets, createdTarget{tag: tag, remove: s.outbounds.Remove})
		out, found = s.outbounds.Outbound(tag)
	case EvaluationTargetEndpoint:
		options, ok := target.Options.(option.Endpoint)
		if !ok {
			return nil, fmt.Errorf("endpoint target holds %T", target.Options)
		}
		if err := s.endpoints.Create(ctx, s.router, s.logger, tag, options.Type, options.Options); err != nil {
			return nil, err
		}
		s.assignmentTargets = append(s.assignmentTargets, createdTarget{tag: tag, remove: s.endpoints.Remove})
		var endpoint A.Endpoint
		endpoint, found = s.endpoints.Get(tag)
		out = endpoint
	default:
		return nil, fmt.Errorf("unknown target type %q", target.Type)
	}
	if !found {
		return nil, fmt.Errorf("created %s %q is unavailable", target.Type, tag)
	}
	return out, nil
}

func (s *Service) closeAssignmentOutbounds() error {
	s.assignmentMu.Lock()
	defer s.assignmentMu.Unlock()
	var err error
	for i := len(s.assignmentTargets) - 1; i >= 0; i-- {
		target := s.assignmentTargets[i]
		if removeErr := target.remove(target.tag); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove assignment target %q: %w", target.tag, removeErr))
		} else {
			s.logger.Debug("outbound evaluation target removed")
		}
	}
	s.assignmentTargets = nil
	return err
}
