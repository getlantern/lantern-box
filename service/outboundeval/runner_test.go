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
)

// testRetryBase is the retry backoff's base wait and its cap both, which holds
// every retry to [800ms, 1s]: the cap applies to the wait once jittered.
const testRetryBase = time.Second

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
			require.NoError(t, json.NewEncoder(w).Encode(Attestation{
				Token:      fmt.Sprintf("attestation-%d", request.WindowIndex),
				ServerTime: time.Now().UTC(),
			}))
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
	s, _ := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.NoError(t, s.runCycle(context.Background()))

	reports := api.reports()
	require.Len(t, reports, 1)
	assignment := serverAssignment()
	require.NoError(t, reports[0].validate(assignment.Sample))
	assert.Equal(t, assignment.ID, reports[0].AssignmentID)
	assert.Equal(t, assignment.ReportToken, reports[0].ReportToken)
	assert.NotEmpty(t, reports[0].IdempotencyKey)
}

func TestRunCycleResubmitsAFinishedReport(t *testing.T) {
	api := newControlAPI()
	api.refuseSubmits = 1
	s, _ := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.NoError(t, s.runCycle(context.Background()))

	reports := api.reports()
	require.Len(t, reports, 2, "a measured grid must not be dropped on one refusal")
	assert.Equal(t, reports[0].IdempotencyKey, reports[1].IdempotencyKey)
	assert.Equal(t, reports[0].Windows, reports[1].Windows)
}

func TestRunCycleGivesUpOnAReportAfterBoundedAttempts(t *testing.T) {
	api := newControlAPI()
	api.submitStatus = http.StatusServiceUnavailable
	s, _ := wiredService(t, api.handler(t))
	s.measure = reachableMeasure

	assert.ErrorIs(t, s.runCycle(context.Background()), apiError{status: http.StatusServiceUnavailable})
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
		"missing server time": {
			assignment: func() Assignment {
				assignment := serverAssignment()
				assignment.ServerTime = time.Time{}
				return assignment
			},
			want: ErrInvalidContract,
		},
		"assignment beyond local bounds": {
			assignment: func() Assignment {
				assignment := serverAssignment()
				assignment.Sample.WindowsPerExit = defaultMaxWindows + 1
				return assignment
			},
			want: ErrInvalidContract,
		},
		"grid longer than the assignment lives": {
			assignment: func() Assignment {
				assignment := serverAssignment()
				assignment.ExpiresAt = assignment.ServerTime.Add(time.Minute)
				assignment.Sample.WindowDurationSeconds = 120
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
			s, _ := wiredService(t, api.handler(t))
			s.measure = reachableMeasure

			assert.ErrorIs(t, s.runCycle(context.Background()), test.want)
		})
	}
}

func TestRunCycleWaitsForACredential(t *testing.T) {
	api := newControlAPI()
	s, _ := wiredService(t, api.handler(t))
	s.measure = reachableMeasure
	idle := *s.config.Load()
	idle.Token = ""
	require.NoError(t, s.SetOutboundEvalConfig(idle))

	assert.ErrorIs(t, s.runCycle(context.Background()), errNoToken)
	assert.Empty(t, api.reports())
}

func TestRunCycleSkipsAnOutboundThatWentAway(t *testing.T) {
	api := newControlAPI()
	s, _ := wiredService(t, api.handler(t))
	s.measure = reachableMeasure
	replaced := *s.config.Load()
	replaced.OutboundTag = "replacement"
	require.NoError(t, s.SetOutboundEvalConfig(replaced))

	err := s.runCycle(context.Background())
	assert.ErrorIs(t, err, errOutboundUnavailable)
	assert.ErrorContains(t, err, "replacement")
	assert.Empty(t, api.reports())
}

func TestRunCycleStopsWhenClosing(t *testing.T) {
	api := newControlAPI()
	api.acquireStatus = http.StatusInternalServerError
	s, _ := wiredService(t, api.handler(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, s.runCycle(ctx), context.Canceled)
}

func TestRetryableCycleError(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"success":          {},
		"no assignment":    {err: ErrNoAssignment},
		"no token":         {err: errNoToken},
		"missing outbound": {err: errOutboundUnavailable},
		"invalid contract": {err: ErrInvalidContract},
		"canceled":         {err: context.Canceled},
		"refused":          {err: apiError{status: http.StatusUnauthorized}},
		"rate limited":     {err: apiError{status: http.StatusTooManyRequests}, want: true},
		"unavailable":      {err: apiError{status: http.StatusServiceUnavailable}, want: true},
		"request deadline": {err: context.DeadlineExceeded, want: true},
		"transport error":  {err: errors.New("connection lost"), want: true},
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

func startTestRunner(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	options := testOptions()
	options.PollInterval = badoption.Duration(10 * time.Second)
	options.NoAssignmentInterval = badoption.Duration(3 * time.Second)
	options.MaxRetryBackoff = badoption.Duration(testRetryBase)
	s := newTestService(t, options)
	s.retryBase = testRetryBase
	s.control = &stubOutbound{tag: "direct"}
	s.outbounds = &stubOutboundManager{outbounds: map[string]A.Outbound{
		"candidate": &stubOutbound{tag: "candidate"},
	}}
	s.api = &apiClient{
		http:           &http.Client{Transport: handlerTransport{handler: handler}},
		requestTimeout: time.Duration(s.options.RequestTimeout),
	}
	s.attest = s.api.attest
	s.measure = reachableMeasure
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
				assignment.ServerTime = time.Time{}
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
		startTestRunner(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/assignments" {
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
