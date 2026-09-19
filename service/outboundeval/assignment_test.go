package outboundeval

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// stubOutbound stands in for one arm. Its DialContext connects to address
// whatever destination it is handed, and a stub that is never dialed through
// needs no address.
type stubOutbound struct {
	A.Outbound
	tag     string
	address string
}

func (o *stubOutbound) Tag() string { return o.tag }

func (o *stubOutbound) DialContext(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, o.address)
}

// testCandidate is the outbound under test for a grid run without a cycle to
// resolve one.
func testCandidate() A.Outbound { return &stubOutbound{tag: "candidate"} }

func testOptions() option.OutboundEvalServiceOptions {
	return option.OutboundEvalServiceOptions{
		AcquireURL:  "http://127.0.0.1:1/assignments",
		AttestURL:   "http://127.0.0.1:1/attestations",
		SubmitURL:   "http://127.0.0.1:1/reports",
		Token:       "token",
		CountryCode: "RU",
		OutboundTag: "candidate",
	}
}

func newTestService(t *testing.T, options option.OutboundEvalServiceOptions) *Service {
	t.Helper()
	created, err := NewService(context.Background(), log.NewNOPFactory().Logger(), "eval", options)
	require.NoError(t, err)
	s := created.(*Service)
	s.retryDelay = time.Millisecond
	s.timeService = liveTime{}
	s.control = &stubOutbound{tag: "direct"}
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// recorder captures which arm each measurement went through, in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, tag)
}

func (r *recorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestRunAssignmentFillsTheGridAndAlternatesArms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		calls := &recorder{}
		s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
			calls.record(out.Tag())
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK, BytesRead: 4096}
		}
		s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attested-" + request.Challenge}, nil
		}

		assignment := validAssignment()
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		assert.Equal(t, assignment.ID, report.IdempotencyKey)
		require.Len(t, report.Windows, 2)
		assert.Equal(t, "attested-challenge-0", report.Windows[0].AttestationToken)
		assert.Equal(t, "attested-challenge-1", report.Windows[1].AttestationToken)
		// Even windows lead with the candidate, odd windows with the control.
		assert.Equal(t, []string{
			"candidate", "direct", "candidate", "direct",
			"direct", "candidate", "direct", "candidate",
		}, calls.recorded())
	})
}

func TestRunAssignmentPadsAnUnattestedWindow(t *testing.T) {
	for name, attestErr := range map[string]error{
		"refused":  apiError{status: http.StatusForbidden},
		"unstable": apiError{status: http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newTestService(t, testOptions())
				measured := 0
				s.measure = func(context.Context, A.Outbound, string) Attempt {
					measured++
					return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
				}
				s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
					if request.Challenge == "challenge-1" {
						return Attestation{}, attestErr
					}
					return Attestation{Token: "attestation-0"}, nil
				}

				assignment := validAssignment()
				report := s.runAssignment(context.Background(), testCandidate(), assignment)

				requireCompleteGrid(t, report, assignment.Sample)
				assert.Empty(t, report.Windows[1].AttestationToken)
				assert.NotZero(t, report.Windows[1].ObservedAt, "a padded window is still observed")
				want := failureAttestationRejected
				if name == "unstable" {
					want = failureAttestation
				}
				for _, attempts := range [][]Attempt{
					report.Windows[1].CandidateAttempts, report.Windows[1].ControlAttempts,
				} {
					for _, attempt := range attempts {
						assert.False(t, attempt.Reachable)
						assert.Equal(t, want, attempt.FailureCode)
					}
				}
				// Only the attested window was measured.
				assert.Equal(t, 2*int(assignment.Sample.AttemptsPerWindow), measured)
			})
		})
	}
}

func TestAttestWindowRetriesWhileTheFailureMayPass(t *testing.T) {
	s := newTestService(t, testOptions())
	attempts := 0
	s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
		attempts++
		if attempts < attestAttempts {
			return Attestation{}, apiError{status: http.StatusServiceUnavailable}
		}
		return Attestation{Token: "attestation"}, nil
	}

	assignment := validAssignment()
	attestation, err := s.attestWindow(context.Background(), assignment.Challenges[0])

	require.NoError(t, err)
	assert.Equal(t, "attestation", attestation.Token)
	assert.Equal(t, attestAttempts, attempts)
}

func TestAttestWindowDoesNotRetryARefusal(t *testing.T) {
	s := newTestService(t, testOptions())
	attempts := 0
	s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
		attempts++
		return Attestation{}, apiError{status: http.StatusUnauthorized}
	}

	assignment := validAssignment()
	_, err := s.attestWindow(context.Background(), assignment.Challenges[0])

	require.Error(t, err)
	assert.Equal(t, 1, attempts)
}

func TestRunAssignmentPadsWhenTheWindowRunsOutOfTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		s.measure = func(ctx context.Context, _ A.Outbound, _ string) Attempt {
			if !sleepContext(ctx, 300*time.Millisecond) {
				return Attempt{FailureCode: failureTimeout}
			}
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		assignment.Sample.AttemptsPerWindow = 4
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 0
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		assert.Equal(t, failureWindowDeadline,
			report.Windows[0].CandidateAttempts[int(assignment.Sample.AttemptsPerWindow)-1].FailureCode)
	})
}

func TestRunWindowPadsWhenTheAssignmentEndsDuringTheSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		attested := false
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			attested = true
			return Attestation{Token: "attestation"}, nil
		}
		s.measure = func(context.Context, A.Outbound, string) Attempt {
			t.Error("a window that never opened must not measure")
			return Attempt{}
		}

		assignment := validAssignment()
		assignment.Sample.FreshSessionDelayMS = 5_000
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		report := s.runAssignment(ctx, testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		assert.False(t, attested, "a window that never opened must not be attested")
		for _, window := range report.Windows {
			assert.Empty(t, window.AttestationToken)
			for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
				for _, attempt := range attempts {
					assert.False(t, attempt.Reachable)
					assert.Equal(t, failureWindowDeadline, attempt.FailureCode)
				}
			}
		}
	})
}

func TestRunWindowSeparatesADeadlineFromARejectedAttestation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(ctx context.Context, _ AttestationRequest) (Attestation, error) {
			<-ctx.Done()
			return Attestation{}, ctx.Err()
		}
		s.measure = func(context.Context, A.Outbound, string) Attempt {
			t.Error("an unattested window must not measure")
			return Attempt{}
		}

		assignment := validAssignment()
		assignment.Sample.WindowDurationSeconds = 1

		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			assert.Empty(t, window.AttestationToken)
			for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
				for _, attempt := range attempts {
					assert.False(t, attempt.Reachable)
					assert.Equal(t, failureWindowDeadline, attempt.FailureCode,
						"a deadline is not the server refusing the challenge")
				}
			}
		}
	})
}

func TestRunWindowReportsADeadlineThatLandsBetweenAttestationAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.retryDelay = 2 * time.Second
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{}, apiError{status: http.StatusBadGateway}
		}
		s.measure = func(context.Context, A.Outbound, string) Attempt {
			t.Error("an unattested window must not measure")
			return Attempt{}
		}

		assignment := validAssignment()
		assignment.Sample.WindowDurationSeconds = 1

		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
				for _, attempt := range attempts {
					assert.Equal(t, failureWindowDeadline, attempt.FailureCode,
						"a window that ran out mid-retry is not an attestation failure")
				}
			}
		}
	})
}

func TestRunWindowDoesNotSpendTheWindowOnItsSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		s.measure = func(context.Context, A.Outbound, string) Attempt {
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 1200
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			for _, attempt := range window.CandidateAttempts {
				assert.True(t, attempt.Reachable, "spacing longer than the window left no time to measure")
			}
		}
	})
}

func TestRunWindowDoesNotBlameAnArmForTheWindowsOwnBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		// Each arm takes most of the window, so the second one is always the one
		// cut short.
		s.measure = func(ctx context.Context, _ A.Outbound, _ string) Attempt {
			if !sleepContext(ctx, 600*time.Millisecond) {
				return Attempt{FailureCode: failureTimeout}
			}
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		assignment.Sample.AttemptsPerWindow = 1
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 0
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
				for _, attempt := range attempts {
					assert.False(t, attempt.Reachable)
					assert.Equal(t, failureWindowDeadline, attempt.FailureCode,
						"neither arm is credited with the window running out of time")
				}
			}
		}
	})
}

func TestRunWindowKeepsACompletedVerdictAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		// The seam ignores the window context, so both arms reach a verdict of
		// their own even though the window ends during the pair.
		s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
			time.Sleep(600 * time.Millisecond)
			if out.Tag() == "candidate" {
				return Attempt{HTTPStatus: http.StatusForbidden, FailureCode: failureHTTPStatus}
			}
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		assignment.Sample.AttemptsPerWindow = 1
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 0
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			assert.Equal(t, failureHTTPStatus, window.CandidateAttempts[0].FailureCode,
				"a completed verdict survives the deadline landing after it")
			assert.True(t, window.ControlAttempts[0].Reachable)
		}
	})
}

func TestRunAssignmentStampsObservationOnTheBoxsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		corrected := time.Now().UTC().Add(90 * time.Minute)
		s.timeService = fixedTime{at: corrected}
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		s.measure = func(context.Context, A.Outbound, string) Attempt {
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		report := s.runAssignment(context.Background(), testCandidate(), assignment)

		assert.Equal(t, corrected, report.Windows[0].ObservedAt)
	})
}
