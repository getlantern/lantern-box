package outboundeval

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// stubOutbound stands in for one eval target. Its DialContext connects to
// address whatever destination it is handed, and a stub that is never dialed
// through needs no address.
type stubOutbound struct {
	A.Outbound
	tag     string
	address string
}

func (o *stubOutbound) Tag() string { return o.tag }

func (o *stubOutbound) DialContext(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, o.address)
}

func testCandidate() A.Outbound { return &stubOutbound{tag: "candidate"} }

func testControl() A.Outbound { return &stubOutbound{tag: "direct"} }

func testOptions() option.OutboundEvalServiceOptions {
	return option.OutboundEvalServiceOptions{
		AcquireURL:  "https://127.0.0.1:1/assignments",
		AttestURL:   "https://127.0.0.1:1/attestations",
		SubmitURL:   "https://127.0.0.1:1/reports",
		Token:       "token",
		CountryCode: "RU",
		// Disables the spacing floor so only the sample's own spacing applies.
		MinFreshSessionDelay: badoption.Duration(time.Nanosecond),
	}
}

func newTestService(t *testing.T, options option.OutboundEvalServiceOptions) *Service {
	t.Helper()
	created, err := NewService(targetContext(context.Background()), log.NewNOPFactory().Logger(), "eval", options)
	require.NoError(t, err)
	s := created.(*Service)
	s.retryDelay = time.Millisecond
	s.timeService = liveTime{}
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// recorder collects strings from concurrent callers.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, value)
}

func (r *recorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestRunAssignmentMeasuresEachPairTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		calls := &recorder{}
		s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
			calls.record(out.Tag())
			time.Sleep(time.Second)
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK, BytesRead: 4096}
		}
		s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attested-" + request.Challenge}, nil
		}

		assignment := validAssignment()
		start := time.Now()
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
		requireCompleteGrid(t, report, assignment.Sample)
		assert.Equal(t, assignment.ID, report.IdempotencyKey)
		require.Len(t, report.Windows, 2)
		assert.Equal(t, "attested-challenge-0", report.Windows[0].AttestationToken)
		assert.Equal(t, "attested-challenge-1", report.Windows[1].AttestationToken)
		pairs := int(assignment.Sample.WindowsPerExit * assignment.Sample.AttemptsPerWindow)
		spacing := time.Duration(assignment.Sample.WindowsPerExit) * assignment.Sample.freshSessionDelay()
		assert.Equal(t, time.Duration(pairs)*time.Second+spacing, time.Since(start),
			"each pair takes as long as one measurement")
		assert.ElementsMatch(t, append(slices.Repeat([]string{"candidate"}, pairs),
			slices.Repeat([]string{"direct"}, pairs)...), calls.recorded())
	})
}

func TestRunAssignmentMeasurementURL(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "omitted", body: `{}`, want: "https://www.wikipedia.org/"},
		{name: "empty", body: `{"measurement_url":""}`, want: "https://www.wikipedia.org/"},
		{name: "override", body: `{"measurement_url":"https://measure.example/resource"}`, want: "https://measure.example/resource"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var decoded Assignment
				require.NoError(t, json.Unmarshal([]byte(test.body), &decoded))
				assignment := validAssignment()
				assignment.MeasurementURL = decoded.MeasurementURL
				require.NoError(t, assignment.validate(fixedNow, testBounds()))
				s := newTestService(t, testOptions())
				s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
					return Attestation{Token: "attested"}, nil
				}
				calls := &recorder{}
				s.measure = func(_ context.Context, out A.Outbound, target string) Attempt {
					assert.Equal(t, test.want, target)
					calls.record(out.Tag())
					return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
				}

				report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

				require.NoError(t, err)
				requireCompleteGrid(t, report, assignment.Sample)
				wantCalls := int(assignment.Sample.WindowsPerExit * assignment.Sample.AttemptsPerWindow)
				assert.ElementsMatch(t, append(slices.Repeat([]string{"candidate"}, wantCalls),
					slices.Repeat([]string{"direct"}, wantCalls)...), calls.recorded())
				encoded, err := json.Marshal(decoded)
				require.NoError(t, err)
				if test.name != "override" {
					assert.NotContains(t, string(encoded), "measurement_url")
				}
			})
		})
	}
}

func TestRunAssignmentStopsAtAnUnattestedWindow(t *testing.T) {
	for name, attestErr := range map[string]error{
		"refused":  apiError{status: http.StatusForbidden},
		"unstable": apiError{status: http.StatusBadGateway},
		"no token": nil,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newTestService(t, testOptions())
				var measured atomic.Int32
				s.measure = func(context.Context, A.Outbound, string) Attempt {
					measured.Add(1)
					return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
				}
				challenges := &recorder{}
				s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
					challenges.record(request.Challenge)
					return Attestation{}, attestErr
				}

				_, err := s.runAssignment(context.Background(), testCandidate(), testControl(), validAssignment())

				require.ErrorIs(t, err, errUnattestedWindow)
				if attestErr != nil {
					assert.ErrorIs(t, err, attestErr)
				} else {
					assert.ErrorIs(t, err, errEmptyAttestation)
				}
				assert.Zero(t, measured.Load(), "an unattested window is not measured")
				assert.NotContains(t, challenges.recorded(), "challenge-1",
					"no window after an unattested one is attempted")
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
		assignment.Sample.AttemptsPerWindow = 5
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 0
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
		requireCompleteGrid(t, report, assignment.Sample)
		for _, attempts := range [][]Attempt{report.Windows[0].CandidateAttempts, report.Windows[0].ControlAttempts} {
			assert.True(t, attempts[2].Reachable)
			assert.Equal(t, failureTimeout, attempts[3].FailureCode, "the deadline cut this attempt short")
			assert.Equal(t, failureWindowDeadline, attempts[4].FailureCode, "the window ended before this attempt")
		}
	})
}

func TestRunAssignmentGivesUpWhenItEndsDuringTheSpacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			t.Error("a window that never opened must not be attested")
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

		_, err := s.runAssignment(ctx, testCandidate(), testControl(), assignment)

		require.ErrorIs(t, err, errAssignmentEnded)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestRunAssignmentGivesUpWhenAttestationOutlastsTheWindow(t *testing.T) {
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

		_, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.ErrorIs(t, err, errUnattestedWindow)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
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
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			for _, attempt := range window.CandidateAttempts {
				assert.True(t, attempt.Reachable, "spacing longer than the window left no time to measure")
			}
		}
	})
}

// A candidate that times out on every attempt leaves its control the whole
// window, so the window counts against the candidate rather than reading as
// inconclusive.
func TestRunWindowLetsAControlOutlastADeadCandidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestService(t, testOptions())
		s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
			return Attestation{Token: "attestation"}, nil
		}
		s.measure = func(ctx context.Context, out A.Outbound, _ string) Attempt {
			if out.Tag() != "candidate" {
				if ctx.Err() != nil {
					return Attempt{FailureCode: failureTimeout}
				}
				return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
			}
			ctx, cancel := context.WithTimeout(ctx, time.Duration(s.options.RequestTimeout))
			defer cancel()
			<-ctx.Done()
			return Attempt{FailureCode: failureTimeout}
		}

		assignment := validAssignment()
		assignment.Sample.AttemptsPerWindow = 3
		assignment.Sample.WindowDurationSeconds = 45
		assignment.Sample.FreshSessionDelayMS = 0
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
		requireCompleteGrid(t, report, assignment.Sample)
		for _, window := range report.Windows {
			for _, attempt := range window.CandidateAttempts {
				assert.Equal(t, failureTimeout, attempt.FailureCode)
			}
			for _, attempt := range window.ControlAttempts {
				assert.True(t, attempt.Reachable)
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
		// The seam ignores the window context, so both eval targets reach a
		// verdict of their own even though the window ends during the pair.
		s.measure = func(_ context.Context, out A.Outbound, _ string) Attempt {
			time.Sleep(1200 * time.Millisecond)
			if out.Tag() == "candidate" {
				return Attempt{HTTPStatus: http.StatusForbidden, FailureCode: failureHTTPStatus}
			}
			return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
		}

		assignment := validAssignment()
		assignment.Sample.AttemptsPerWindow = 1
		assignment.Sample.WindowDurationSeconds = 1
		assignment.Sample.FreshSessionDelayMS = 0
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
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
		report, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

		require.NoError(t, err)
		assert.Equal(t, corrected, report.Windows[0].ObservedAt)
	})
}

func TestRunWindowWaitsAtLeastTheMinimumSpacing(t *testing.T) {
	for name, test := range map[string]struct {
		requested, minimum, want time.Duration
	}{
		"sample asks for less": {requested: time.Second, minimum: 30 * time.Second, want: 30 * time.Second},
		"sample asks for more": {requested: 40 * time.Second, minimum: 30 * time.Second, want: 40 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				options := testOptions()
				options.MinFreshSessionDelay = badoption.Duration(test.minimum)
				s := newTestService(t, options)
				s.attest = func(context.Context, AttestationRequest) (Attestation, error) {
					return Attestation{Token: "attestation"}, nil
				}
				s.measure = func(context.Context, A.Outbound, string) Attempt {
					return Attempt{Reachable: true, HTTPStatus: http.StatusOK}
				}

				assignment := validAssignment()
				assignment.Sample.FreshSessionDelayMS = uint32(test.requested / time.Millisecond)
				start := time.Now()
				_, err := s.runAssignment(context.Background(), testCandidate(), testControl(), assignment)

				require.NoError(t, err)
				assert.Equal(t, time.Duration(assignment.Sample.WindowsPerExit)*test.want, time.Since(start))
			})
		})
	}
}

func TestRunAssignmentReportsAnAssignmentThatEndsDuringAttestation(t *testing.T) {
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
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_, err := s.runAssignment(ctx, testCandidate(), testControl(), validAssignment())

		require.ErrorIs(t, err, errAssignmentEnded)
		assert.NotErrorIs(t, err, errUnattestedWindow)
	})
}
