package outboundeval

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
)

func (s *Service) measureAssignment(candidateTag string, assignment Assignment) (report Report, err error) {
	now := s.timeService.TimeFunc()()
	ctx, cancel := context.WithTimeout(s.ctx, assignment.ExpiresAt.Sub(now))
	defer cancel()
	var candidate A.Outbound
	control := s.control
	if assignment.Candidate != nil && assignment.Control != nil {
		defer func() {
			err = errors.Join(err, s.closeAssignmentOutbounds())
		}()
		candidate, control, err = s.createAssignmentOutbounds(ctx, assignment)
		if err != nil {
			return Report{}, err
		}
	} else {
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
	var arms [2]A.Outbound
	for i, options := range []*option.Outbound{assignment.Candidate, assignment.Control} {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Server-provided tags must not replace configured outbounds.
		tag := "outbound-eval-" + rand.Text()
		if err := s.outbounds.Create(ctx, s.router, s.logger, tag, options.Type, options.Options); err != nil {
			return nil, nil, fmt.Errorf("create assignment outbound %d: %w", i, err)
		}
		s.assignmentTags = append(s.assignmentTags, tag)
		var found bool
		arms[i], found = s.outbounds.Outbound(tag)
		if !found {
			return nil, nil, fmt.Errorf("created assignment outbound %q is unavailable", tag)
		}
	}
	return arms[0], arms[1], nil
}

func (s *Service) closeAssignmentOutbounds() error {
	s.assignmentMu.Lock()
	defer s.assignmentMu.Unlock()
	var err error
	for i := len(s.assignmentTags) - 1; i >= 0; i-- {
		tag := s.assignmentTags[i]
		if removeErr := s.outbounds.Remove(tag); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove assignment outbound %q: %w", tag, removeErr))
		}
	}
	s.assignmentTags = nil
	return err
}
