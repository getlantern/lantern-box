package outboundeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// testRetryBase is the retry backoff's base wait and its cap both, which holds
// every retry to [800ms, 1s]: the cap applies to the wait once jittered.
const testRetryBase = time.Second

// fixedTime and liveTime stand in for the box's time service, which Start
// resolves and these tests wire by hand.
type fixedTime struct{ at time.Time }

func (f fixedTime) TimeFunc() func() time.Time { return func() time.Time { return f.at } }

type liveTime struct{}

func (liveTime) TimeFunc() func() time.Time { return time.Now }

// controlAPI answers the acquire, attest and submit endpoints.
type controlAPI struct {
	mu            sync.Mutex
	acquireStatus int
	submitStatus  int
	// refuseSubmits is how many submissions are refused as unavailable before
	// submitStatus applies.
	refuseSubmits int
	assignment    func() Assignment
	submitted     []Report
}

func newControlAPI() *controlAPI {
	return &controlAPI{assignment: serverAssignment}
}

func (c *controlAPI) reports() []Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Report(nil), c.submitted...)
}

func (c *controlAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/assignments"):
			c.mu.Lock()
			status, assignment := c.acquireStatus, c.assignment
			c.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			require.NoError(t, json.NewEncoder(w).Encode(assignment()))
		case strings.HasSuffix(r.URL.Path, "/attestations"):
			var request AttestationRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.NoError(t, json.NewEncoder(w).Encode(Attestation{Token: "attested-" + request.Challenge}))
		case strings.HasSuffix(r.URL.Path, "/reports"):
			var report Report
			require.NoError(t, json.NewDecoder(r.Body).Decode(&report))
			c.mu.Lock()
			c.submitted = append(c.submitted, report)
			status := c.submitStatus
			if c.refuseSubmits > 0 {
				c.refuseSubmits--
				status = http.StatusServiceUnavailable
			}
			c.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func reachableMeasure(context.Context, A.Outbound, string) Attempt {
	return Attempt{Reachable: true, HTTPStatus: http.StatusOK, BytesRead: 2048}
}

func TestRunCycleMeasuresAndSubmitsACompleteGrid(t *testing.T) {
	api := newControlAPI()
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.NoError(t, s.runCycle())

	reports := api.reports()
	require.Len(t, reports, 1)
	assignment := serverAssignment()
	requireCompleteGrid(t, reports[0], assignment.Sample)
	assert.Equal(t, assignment.ReportToken, reports[0].ReportToken)
	assert.Equal(t, assignment.ID, reports[0].IdempotencyKey, "the assignment identifies the report through its key")
}

func TestRunCycleAcquisitionKeys(t *testing.T) {
	for _, test := range []struct {
		name          string
		acquireStatus int
		refuseSubmits int
		wantErr       error
		reuseKey      bool
	}{
		{name: "completed"},
		{
			name: "acquisition retry", acquireStatus: http.StatusBadGateway,
			wantErr: apiError{status: http.StatusBadGateway}, reuseKey: true,
		},
		{
			name: "rate limited", acquireStatus: http.StatusTooManyRequests,
			wantErr: apiError{status: http.StatusTooManyRequests}, reuseKey: true,
		},
		{
			name: "no assignment", acquireStatus: http.StatusServiceUnavailable,
			wantErr: ErrNoAssignment,
		},
		{
			name: "refused", acquireStatus: http.StatusUnauthorized,
			wantErr: apiError{status: http.StatusUnauthorized},
		},
		{
			name: "submission exhausted retries", refuseSubmits: submitAttempts,
			wantErr: apiError{status: http.StatusServiceUnavailable},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newControlAPI()
			api.refuseSubmits = test.refuseSubmits
			handler := api.handler(t)
			var requests []AssignmentRequest
			s := wireRunner(t, testOptions(), func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/assignments" {
					var request AssignmentRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					requests = append(requests, request)
					if len(requests) == 1 && test.acquireStatus != 0 {
						w.WriteHeader(test.acquireStatus)
						return
					}
				}
				handler(w, r)
			})

			require.ErrorIs(t, s.runCycle(), test.wantErr)
			require.NoError(t, s.runCycle())

			require.Len(t, requests, 2)
			for _, request := range requests {
				require.NotEmpty(t, request.IdempotencyKey)
				assert.LessOrEqual(t, len(request.IdempotencyKey), 128)
				assert.NotEqual(t, s.config.Load().Token, request.IdempotencyKey)
			}
			if test.reuseKey {
				assert.Equal(t, requests[0], requests[1])
			} else {
				assert.NotEqual(t, requests[0].IdempotencyKey, requests[1].IdempotencyKey)
			}
		})
	}
}

func TestRunCycleAcquisitionKeyTracksConfig(t *testing.T) {
	for _, field := range []string{"token", "country", "unchanged"} {
		t.Run(field, func(t *testing.T) {
			var requests []AssignmentRequest
			s := wireRunner(t, testOptions(), func(w http.ResponseWriter, r *http.Request) {
				var request AssignmentRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				requests = append(requests, request)
				w.WriteHeader(http.StatusBadGateway)
			})

			require.Error(t, s.runCycle())
			config := *s.config.Load()
			switch field {
			case "token":
				config.Token = "rotated-token"
			case "country":
				config.CountryCode = "IR"
			}
			require.NoError(t, s.SetOutboundEvalConfig(config))
			require.Error(t, s.runCycle())

			require.Len(t, requests, 2)
			if field == "unchanged" {
				assert.Equal(t, requests[0], requests[1])
			} else {
				assert.NotEqual(t, requests[0].IdempotencyKey, requests[1].IdempotencyKey)
			}
			assert.Equal(t, config.CountryCode, requests[1].CountryCode)
		})
	}
}

func TestRunCycleRefusesAnExpiryTheBoxsClockRejects(t *testing.T) {
	api := newControlAPI()
	s := wiredService(t, api.handler(t))
	s.timeService = fixedTime{at: time.Now().Add(time.Hour)}
	s.measure = reachableMeasure

	assert.ErrorIs(t, s.runCycle(), ErrInvalidContract)
	assert.Empty(t, api.reports())
}

// A window longer than the whole grid takes still measures every attempt: the
// window's duration is a budget, not a wait.
func TestRunCycleAcceptsAWindowLongerThanItNeeds(t *testing.T) {
	api := newControlAPI()
	api.assignment = func() Assignment {
		assignment := serverAssignment()
		assignment.Sample.WindowDurationSeconds = 120
		return assignment
	}
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	require.NoError(t, s.runCycle())

	reports := api.reports()
	require.Len(t, reports, 1)
	requireCompleteGrid(t, reports[0], api.assignment().Sample)
	for _, window := range reports[0].Windows {
		for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
			for _, attempt := range attempts {
				assert.True(t, attempt.Reachable)
			}
		}
	}
}

func TestRunCycleMeasuresOnlyUntilTheAssignmentExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assignment := validAssignment()
		assignment.ExpiresAt = time.Now().Add(time.Second)
		assignment.Sample.WindowsPerExit = defaultMaxWindows
		assignment.Sample.FreshSessionDelayMS = maxFreshSessionDelayMS
		assignment.Challenges = make([]WindowChallenge, defaultMaxWindows)
		for i := range assignment.Challenges {
			assignment.Challenges[i] = WindowChallenge{WindowIndex: uint32(i), Challenge: "challenge"}
		}
		api := newControlAPI()
		api.assignment = func() Assignment { return assignment }
		s := wireRunner(t, testOptions(), api.handler(t))

		start := time.Now()
		require.ErrorIs(t, s.runCycle(), errUnattestedReport)
		assert.Equal(t, time.Second, time.Since(start))

		assert.Empty(t, api.reports())
	})
}

func TestRunCycleResubmitsAFinishedReport(t *testing.T) {
	api := newControlAPI()
	api.refuseSubmits = 1
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.NoError(t, s.runCycle())

	reports := api.reports()
	require.Len(t, reports, 2, "a measured grid must not be dropped on one refusal")
	assert.Equal(t, reports[0].IdempotencyKey, reports[1].IdempotencyKey)
	assert.Equal(t, reports[0].Windows, reports[1].Windows)
}

func TestRunCycleKeepsItsCredentialForSubmissionRetries(t *testing.T) {
	api := newControlAPI()
	api.refuseSubmits = 1
	handler := api.handler(t)
	var authorizations []string
	s := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reports") {
			authorizations = append(authorizations, r.Header.Get("Authorization"))
		}
		handler(w, r)
	})
	original := *s.config.Load()
	s.measure = func(ctx context.Context, out A.Outbound, target string) Attempt {
		updated := original
		updated.Token = "rotated-token"
		require.NoError(t, s.SetOutboundEvalConfig(updated))
		return reachableMeasure(ctx, out, target)
	}

	require.NoError(t, s.runCycle())

	assert.Equal(t, []string{"Bearer " + original.Token, "Bearer " + original.Token}, authorizations)
	assert.Equal(t, "rotated-token", s.config.Load().Token)
}

func TestRunCycleDoesNotSubmitUnattestedWindows(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "attestation refused", err: apiError{status: http.StatusForbidden}},
		{name: "attestation failed", err: errors.New("connection lost")},
		{name: "empty attestation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newControlAPI()
			s := wiredService(t, api.handler(t))
			s.measure = reachableMeasure
			s.attest = func(_ context.Context, request AttestationRequest) (Attestation, error) {
				if request.Challenge == "challenge-0" {
					return Attestation{Token: "attested-first-window"}, nil
				}
				return Attestation{}, test.err
			}

			require.ErrorIs(t, s.runCycle(), errUnattestedReport)
			assert.Empty(t, api.reports())
		})
	}
}

func TestRunCycleSubmitsAttestedDeadlineFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newControlAPI()
		s := wireRunner(t, testOptions(), api.handler(t))
		s.measure = func(ctx context.Context, _ A.Outbound, _ string) Attempt {
			<-ctx.Done()
			return Attempt{FailureCode: failureTimeout}
		}

		require.NoError(t, s.runCycle())

		reports := api.reports()
		require.Len(t, reports, 1)
		requireCompleteGrid(t, reports[0], serverAssignment().Sample)
		for _, window := range reports[0].Windows {
			require.NotEmpty(t, window.AttestationToken)
			for _, attempts := range [][]Attempt{window.CandidateAttempts, window.ControlAttempts} {
				for _, attempt := range attempts {
					assert.Equal(t, failureWindowDeadline, attempt.FailureCode)
				}
			}
		}
	})
}

func TestRunCycleGivesUpOnAReportAfterBoundedAttempts(t *testing.T) {
	api := newControlAPI()
	api.submitStatus = http.StatusServiceUnavailable
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.ErrorIs(t, s.runCycle(), apiError{status: http.StatusServiceUnavailable})
	assert.Len(t, api.reports(), submitAttempts)
}

func TestRunCycleErrors(t *testing.T) {
	for name, test := range map[string]struct {
		acquireStatus int
		submitStatus  int
		assignment    func() Assignment
		want          error
	}{
		"nothing to measure": {acquireStatus: http.StatusServiceUnavailable, want: ErrNoAssignment},
		"credential refused": {acquireStatus: http.StatusUnauthorized, want: apiError{status: http.StatusUnauthorized}},
		"bad request":        {acquireStatus: http.StatusBadRequest, want: apiError{status: http.StatusBadRequest}},
		"server trouble":     {acquireStatus: http.StatusInternalServerError, want: apiError{status: http.StatusInternalServerError}},
		"rate limited":       {acquireStatus: http.StatusTooManyRequests, want: apiError{status: http.StatusTooManyRequests}},
		"submit refused":     {submitStatus: http.StatusBadRequest, want: apiError{status: http.StatusBadRequest}},
		"submit conflict":    {submitStatus: http.StatusConflict, want: apiError{status: http.StatusConflict}},
		"assignment beyond local bounds": {
			assignment: func() Assignment {
				assignment := serverAssignment()
				assignment.Sample.WindowsPerExit = defaultMaxWindows + 1
				return assignment
			},
			want: ErrInvalidContract,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newControlAPI()
			api.acquireStatus = test.acquireStatus
			api.submitStatus = test.submitStatus
			if test.assignment != nil {
				api.assignment = test.assignment
			}
			s := wiredService(t, api.handler(t))
			s.measure = reachableMeasure

			assert.ErrorIs(t, s.runCycle(), test.want)
		})
	}
}

func TestRunCycleWaitsForACredential(t *testing.T) {
	api := newControlAPI()
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure
	idle := *s.config.Load()
	idle.Token = ""
	require.NoError(t, s.SetOutboundEvalConfig(idle))

	assert.ErrorIs(t, s.runCycle(), errNoToken)
	assert.Empty(t, api.reports())
}

func TestRunCycleSkipsAnOutboundThatWentAway(t *testing.T) {
	api := newControlAPI()
	s := wiredService(t, api.handler(t))
	s.measure = reachableMeasure
	replaced := *s.config.Load()
	replaced.OutboundTag = "replacement"
	require.NoError(t, s.SetOutboundEvalConfig(replaced))

	err := s.runCycle()
	assert.ErrorIs(t, err, errOutboundUnavailable)
	assert.ErrorContains(t, err, "replacement")
	assert.Empty(t, api.reports())
}

func TestRunCycleStopsWhenClosing(t *testing.T) {
	api := newControlAPI()
	api.acquireStatus = http.StatusInternalServerError
	s := wiredService(t, api.handler(t))

	s.cancel()
	assert.ErrorIs(t, s.runCycle(), context.Canceled)
}

type cancellationTransport struct {
	http.RoundTripper
	path    string
	started chan struct{}
}

func (t cancellationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path != t.path {
		return t.RoundTripper.RoundTrip(request)
	}
	close(t.started)
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestCloseCancelsCycleAPIRequests(t *testing.T) {
	for _, path := range []string{"/assignments", "/reports"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newControlAPI()
				s := wireRunner(t, testOptions(), api.handler(t))
				started := make(chan struct{})
				s.api.http.Transport = cancellationTransport{
					RoundTripper: s.api.http.Transport, path: path, started: started,
				}
				done := make(chan error, 1)
				go func() { done <- s.runCycle() }()
				<-started

				require.NoError(t, s.Close())

				require.ErrorIs(t, <-done, context.Canceled)
				assert.Empty(t, api.reports())
			})
		})
	}
}

func TestRetryableCycleError(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"success":           {},
		"no assignment":     {err: ErrNoAssignment},
		"no token":          {err: errNoToken},
		"missing outbound":  {err: errOutboundUnavailable},
		"invalid contract":  {err: ErrInvalidContract},
		"unattested report": {err: errUnattestedReport},
		"canceled":          {err: context.Canceled},
		"refused":           {err: apiError{status: http.StatusUnauthorized}},
		"rate limited":      {err: apiError{status: http.StatusTooManyRequests}, want: true},
		"unavailable":       {err: apiError{status: http.StatusServiceUnavailable}, want: true},
		"request deadline":  {err: context.DeadlineExceeded, want: true},
		"transport error":   {err: errors.New("connection lost"), want: true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.want, retryableCycleError(test.err))
			if test.err != nil {
				assert.Equal(t, test.want, retryableCycleError(fmt.Errorf("cycle: %w", test.err)))
			}
		})
	}
}

// handlerTransport answers a request in the goroutine that made it. A synctest
// bubble advances its clock only while every goroutine in it is durably
// blocked, which a goroutine waiting on a real network never is.
type handlerTransport struct {
	handler http.Handler
}

func (h handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

// wireRunner is a service wired as Start would wire it, answering the control
// API in the goroutine that called it, without the cycle loop running.
func wireRunner(t *testing.T, options option.OutboundEvalServiceOptions, handler http.HandlerFunc) *Service {
	t.Helper()
	s := newTestService(t, options)
	s.outbounds = &stubOutboundManager{outbounds: map[string]A.Outbound{
		"candidate": &stubOutbound{tag: "candidate"},
	}}
	s.api = &apiClient{
		ctx:            s.ctx,
		http:           &http.Client{Transport: handlerTransport{handler: handler}},
		acquireURL:     s.options.AcquireURL,
		attestURL:      s.options.AttestURL,
		submitURL:      s.options.SubmitURL,
		requestTimeout: time.Duration(s.options.RequestTimeout),
	}
	s.attest = s.api.attest
	s.measure = reachableMeasure
	return s
}

func startTestRunner(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	options := testOptions()
	options.PollInterval = badoption.Duration(10 * time.Second)
	options.NoAssignmentInterval = badoption.Duration(3 * time.Second)
	options.MaxRetryBackoff = badoption.Duration(testRetryBase)
	s := wireRunner(t, options, handler)
	s.retryBase = testRetryBase
	s.started.Store(true)
	go s.run()
	return s
}

func TestRunPollingIntervals(t *testing.T) {
	for name, test := range map[string]struct {
		status     int
		assignment func() Assignment
		wait       time.Duration
	}{
		"completed":     {wait: 10 * time.Second},
		"no assignment": {status: http.StatusServiceUnavailable, wait: 3 * time.Second},
		"refused":       {status: http.StatusUnauthorized, wait: 3 * time.Second},
		"invalid": {
			assignment: func() Assignment {
				assignment := serverAssignment()
				assignment.Sample.WindowsPerExit = defaultMaxWindows + 1
				return assignment
			},
			wait: 3 * time.Second,
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newControlAPI()
				api.acquireStatus = test.status
				assignment := serverAssignment
				if test.assignment != nil {
					assignment = test.assignment
				}
				api.assignment = func() Assignment {
					served := assignment()
					served.Sample.FreshSessionDelayMS = 0
					return served
				}
				handler := api.handler(t)
				var acquired atomic.Int64
				startTestRunner(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/assignments" {
						acquired.Add(1)
					}
					handler(w, r)
				})
				synctest.Wait()
				time.Sleep(9 * time.Second)
				synctest.Wait()
				assert.Zero(t, acquired.Load())
				time.Sleep(time.Second)
				synctest.Wait()
				require.EqualValues(t, 1, acquired.Load())

				time.Sleep(test.wait - time.Nanosecond)
				synctest.Wait()
				assert.EqualValues(t, 1, acquired.Load())
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				assert.EqualValues(t, 2, acquired.Load())
			})
		})
	}
}

func TestRunBackoffRetriesWithoutWaitingForTheTicker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newControlAPI()
		api.assignment = func() Assignment {
			assignment := serverAssignment()
			assignment.Sample.FreshSessionDelayMS = 0
			return assignment
		}
		handler := api.handler(t)
		var acquired, retriedAt atomic.Int64
		var requests []AssignmentRequest
		startTestRunner(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/assignments" {
				var request AssignmentRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				requests = append(requests, request)
				switch acquired.Add(1) {
				case 1:
					w.WriteHeader(http.StatusBadGateway)
					return
				case 2:
					retriedAt.Store(time.Now().UnixNano())
				}
			}
			handler(w, r)
		})
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, acquired.Load())

		// Jitter lands the retry in the last fifth of the capped wait, long
		// before the tick that would otherwise start the next cycle.
		time.Sleep(testRetryBase*4/5 - time.Nanosecond)
		synctest.Wait()
		require.EqualValues(t, 1, acquired.Load())
		time.Sleep(testRetryBase/5 + time.Nanosecond)
		synctest.Wait()
		require.EqualValues(t, 2, acquired.Load())

		// The cycle after a retried one waits a whole poll interval.
		time.Sleep(time.Until(time.Unix(0, retriedAt.Load()).Add(10*time.Second)) - time.Nanosecond)
		synctest.Wait()
		assert.EqualValues(t, 2, acquired.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.EqualValues(t, 3, acquired.Load())
		require.Len(t, requests, 3)
		assert.NotEmpty(t, requests[0].IdempotencyKey)
		assert.Equal(t, requests[0], requests[1])
		assert.NotEqual(t, requests[1].IdempotencyKey, requests[2].IdempotencyKey)
	})
}

func TestRunWaitsAfterALongCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var acquired atomic.Int64
		startTestRunner(t, func(w http.ResponseWriter, _ *http.Request) {
			if acquired.Add(1) == 1 {
				time.Sleep(15 * time.Second)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, acquired.Load())
		// The tick that elapses while the cycle runs must not start another one.
		time.Sleep(15 * time.Second)
		synctest.Wait()
		assert.EqualValues(t, 1, acquired.Load())
		time.Sleep(3*time.Second - time.Nanosecond)
		synctest.Wait()
		assert.EqualValues(t, 1, acquired.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.EqualValues(t, 2, acquired.Load())
	})
}

func TestRunStopsWhileWaiting(t *testing.T) {
	for _, retrying := range []bool{false, true} {
		t.Run(fmt.Sprintf("retrying=%t", retrying), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var acquired atomic.Int64
				s := startTestRunner(t, func(w http.ResponseWriter, _ *http.Request) {
					acquired.Add(1)
					w.WriteHeader(http.StatusBadGateway)
				})
				if retrying {
					s.wake <- struct{}{}
				}
				synctest.Wait()
				if retrying {
					require.EqualValues(t, 1, acquired.Load())
				} else {
					require.Zero(t, acquired.Load())
				}
				require.NoError(t, s.Close())
				select {
				case <-s.done:
				default:
					t.Fatal("the runner did not stop")
				}
			})
		})
	}
}

func TestSleepContextReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	assert.True(t, sleepContext(ctx, 0))
	assert.True(t, sleepContext(ctx, time.Millisecond))
	cancel()
	assert.False(t, sleepContext(ctx, 0))
	assert.False(t, sleepContext(ctx, time.Minute))
}
