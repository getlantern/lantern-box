package outboundeval

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type managedTestOutbound struct {
	stubOutbound
	closes atomic.Int32
	closed chan struct{}
}

func (o *managedTestOutbound) Close() error {
	if o.closes.Add(1) == 1 {
		close(o.closed)
	}
	return nil
}

func assertOutboundsClosed(t *testing.T, outbounds []*managedTestOutbound) {
	t.Helper()
	for _, out := range outbounds {
		assert.EqualValues(t, 1, out.closes.Load(), "outbound %q", out.Tag())
	}
}

type assignmentTestManager struct {
	A.OutboundManager
	mu           sync.Mutex
	outbounds    map[string]A.Outbound
	created      []*managedTestOutbound
	options      []any
	createCalls  int
	failCreateAt int
	createErr    error
	removeErr    error
	onCreate     func(context.Context)
}

func (m *assignmentTestManager) Outbound(tag string) (A.Outbound, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, found := m.outbounds[tag]
	return out, found
}

func (m *assignmentTestManager) Create(ctx context.Context, _ A.Router, _ log.ContextLogger, tag, _ string, options any) error {
	if m.onCreate != nil {
		m.onCreate(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	if m.createCalls == m.failCreateAt {
		return m.createErr
	}
	if _, exists := m.outbounds[tag]; exists {
		return errors.New("replacement of an existing outbound")
	}
	out := &managedTestOutbound{stubOutbound: stubOutbound{tag: tag}, closed: make(chan struct{})}
	m.outbounds[tag] = out
	m.created = append(m.created, out)
	m.options = append(m.options, options)
	return nil
}

func (m *assignmentTestManager) Remove(tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, exists := m.outbounds[tag]
	if !exists {
		return errors.New("outbound already removed")
	}
	delete(m.outbounds, tag)
	return errors.Join(out.(*managedTestOutbound).Close(), m.removeErr)
}

func assignmentWithOutbounds() Assignment {
	assignment := serverAssignment()
	assignment.Sample.FreshSessionDelayMS = 0
	assignment.Candidate = &O.Outbound{
		Type: "direct", Tag: "candidate", Options: &O.DirectOutboundOptions{},
	}
	assignment.Control = &O.Outbound{
		Type: "direct", Tag: "direct", Options: &O.DirectOutboundOptions{},
	}
	return assignment
}

func serviceWithAssignmentManager(t *testing.T) (*Service, *assignmentTestManager) {
	t.Helper()
	s := newTestService(t, testOptions())
	manager := &assignmentTestManager{outbounds: map[string]A.Outbound{
		"candidate": testCandidate(), "direct": s.control,
	}}
	s.outbounds = manager
	s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
		return Attestation{Token: "attested-" + request.Challenge}, nil
	}
	return s, manager
}

func TestAssignmentOutboundsRequireBothArms(t *testing.T) {
	for _, test := range []struct {
		name               string
		candidate, control bool
	}{
		{name: "neither"},
		{name: "candidate only", candidate: true},
		{name: "control only", control: true},
		{name: "both", candidate: true, control: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, manager := serviceWithAssignmentManager(t)
			configuredCandidate, _ := manager.Outbound("candidate")
			configuredControl := s.control
			assignment := assignmentWithOutbounds()
			if !test.candidate {
				assignment.Candidate = nil
			}
			if !test.control {
				assignment.Control = nil
			}
			var measured []A.Outbound
			s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
				measured = append(measured, out)
				assert.Same(t, configuredControl, s.control)
				return Attempt{Reachable: true}
			}

			report, err := s.measureAssignment("candidate", assignment)

			require.NoError(t, err)
			require.NoError(t, s.Close())
			requireCompleteGrid(t, report, assignment.Sample)
			candidate, control := configuredCandidate, configuredControl
			if test.candidate && test.control {
				require.Len(t, manager.created, 2)
				assert.Equal(t, []any{assignment.Candidate.Options, assignment.Control.Options}, manager.options)
				candidate, control = manager.created[0], manager.created[1]
				assertOutboundsClosed(t, manager.created)
				for _, out := range manager.created {
					_, found := manager.Outbound(out.Tag())
					assert.False(t, found)
				}
			} else {
				assert.Empty(t, manager.created)
			}
			assert.Equal(t, []A.Outbound{
				candidate, control, candidate, control,
				control, candidate, control, candidate,
			}, measured)
			currentCandidate, _ := manager.Outbound("candidate")
			currentControl, _ := manager.Outbound("direct")
			assert.Same(t, configuredCandidate, currentCandidate)
			assert.Same(t, configuredControl, currentControl)
		})
	}
}

func TestAssignmentOutboundsCleanUpCreationFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			s, manager := serviceWithAssignmentManager(t)
			manager.failCreateAt = failAt
			manager.createErr = errors.New("create failed")
			s.measure = func(context.Context, A.Outbound, string) Attempt {
				t.Fatal("measurement after creation failed")
				return Attempt{}
			}

			_, err := s.measureAssignment("candidate", assignmentWithOutbounds())

			require.ErrorIs(t, err, manager.createErr)
			require.Len(t, manager.created, failAt-1)
			assertOutboundsClosed(t, manager.created)
			assert.Len(t, manager.outbounds, 2)
		})
	}
}

func TestAssignmentOutboundsSurfaceCleanupErrors(t *testing.T) {
	s, manager := serviceWithAssignmentManager(t)
	manager.removeErr = errors.New("close failed")
	s.measure = reachableMeasure

	_, err := s.measureAssignment("candidate", assignmentWithOutbounds())

	require.ErrorIs(t, err, manager.removeErr)
	require.Len(t, manager.created, 2)
	assertOutboundsClosed(t, manager.created)
}

func TestAssignmentOutboundsCleanUpAfterAttestationFailure(t *testing.T) {
	s, manager := serviceWithAssignmentManager(t)
	s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
		return Attestation{}, apiError{status: http.StatusForbidden}
	}
	s.measure = func(context.Context, A.Outbound, string) Attempt {
		t.Fatal("measurement after attestation failed")
		return Attempt{}
	}

	report, err := s.measureAssignment("candidate", assignmentWithOutbounds())

	require.NoError(t, err)
	require.ErrorIs(t, s.submitReport("token", report), errUnattestedReport)
	require.Len(t, manager.created, 2)
	assertOutboundsClosed(t, manager.created)
	assert.Len(t, manager.outbounds, 2)
}

func TestCloseRemovesAssignmentOutboundsDuringMeasurement(t *testing.T) {
	s, manager := serviceWithAssignmentManager(t)
	measuring := make(chan struct{})
	var once sync.Once
	s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
		once.Do(func() { close(measuring) })
		<-out.(*managedTestOutbound).closed
		return Attempt{FailureCode: failureCanceled}
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.measureAssignment("candidate", assignmentWithOutbounds())
		done <- err
	}()
	select {
	case <-measuring:
	case <-time.After(time.Second):
		t.Fatal("measurement did not start")
	}

	require.NoError(t, s.Close())
	require.NoError(t, s.Close())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("measurement did not stop")
	}
	require.Len(t, manager.created, 2)
	assertOutboundsClosed(t, manager.created)
	assert.Len(t, manager.outbounds, 2)
}

func TestCloseDuringAssignmentOutboundCreation(t *testing.T) {
	s, manager := serviceWithAssignmentManager(t)
	creating := make(chan struct{})
	manager.onCreate = func(ctx context.Context) {
		close(creating)
		<-ctx.Done()
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.measureAssignment("candidate", assignmentWithOutbounds())
		done <- err
	}()
	select {
	case <-creating:
	case <-time.After(time.Second):
		t.Fatal("creation did not start")
	}

	require.NoError(t, s.Close())

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("creation did not stop")
	}
	require.Len(t, manager.created, 1)
	assertOutboundsClosed(t, manager.created)
	assert.Len(t, manager.outbounds, 2)
}

func TestAssignmentDeadlineStopsOutboundCreation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, manager := serviceWithAssignmentManager(t)
		s.timeService = fixedTime{at: fixedNow}
		assignment := assignmentWithOutbounds()
		assignment.ExpiresAt = fixedNow.Add(time.Second)
		manager.onCreate = func(ctx context.Context) {
			<-ctx.Done()
		}
		start := time.Now()

		_, err := s.measureAssignment("candidate", assignment)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, time.Second, time.Since(start))
		assert.NoError(t, s.ctx.Err())
		require.Len(t, manager.created, 1)
		assertOutboundsClosed(t, manager.created)
		assert.Len(t, manager.outbounds, 2)
	})
}
