package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sbox "github.com/sagernet/sing-box"
	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/constant"
	lboption "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/service/outboundeval"
)

// controlAPI stands in for the evaluation control plane: it hands out one
// assignment, attests every window, and captures the report.
type controlAPI struct {
	mu          sync.Mutex
	tokens      []string
	challenges  []string
	reports     []outboundeval.Report
	reported    chan struct{}
	reportsOnce sync.Once
}

func (c *controlAPI) assignment() outboundeval.Assignment {
	now := time.Now().UTC().Truncate(time.Second)
	return outboundeval.Assignment{
		ID:             "e2e-assignment",
		ReportToken:    "e2e-report-token",
		MeasurementURL: "https://measure.invalid/resource",
		Sample: outboundeval.SampleSpec{
			WindowsPerExit:        2,
			AttemptsPerWindow:     1,
			WindowDurationSeconds: 5,
		},
		Challenges: []outboundeval.WindowChallenge{
			{WindowIndex: 0, Challenge: "challenge-0"},
			{WindowIndex: 1, Challenge: "challenge-1"},
		},
		ExpiresAt:  now.Add(10 * time.Minute),
		ServerTime: now,
	}
}

func (c *controlAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/assignments"):
		c.mu.Lock()
		c.tokens = append(c.tokens, r.Header.Get("Authorization"))
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(c.assignment())
	case strings.HasSuffix(r.URL.Path, "/attestations"):
		var request outboundeval.AttestationRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		c.mu.Lock()
		c.challenges = append(c.challenges, request.Challenge)
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(outboundeval.Attestation{
			Token:      "attested-" + request.Challenge,
			ServerTime: time.Now().UTC(),
		})
	case strings.HasSuffix(r.URL.Path, "/reports"):
		var report outboundeval.Report
		_ = json.NewDecoder(r.Body).Decode(&report)
		c.mu.Lock()
		c.reports = append(c.reports, report)
		c.mu.Unlock()
		c.reportsOnce.Do(func() { close(c.reported) })
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestOutboundEvalRunsInsideABox drives the service through a real box: the
// registry entry, the option decoding, the outbound lookups at start, and one
// whole cycle onto the wire.
//
// The measurement target does not resolve, so every attempt fails. That is the
// point of the assertion: the grid still has to arrive complete.
func TestOutboundEvalRunsInsideABox(t *testing.T) {
	api := &controlAPI{reported: make(chan struct{})}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	boxCtx := box.BaseContext()
	options := option.Options{
		Log: &option.LogOptions{Disabled: true},
		Outbounds: []option.Outbound{
			{Type: C.TypeDirect, Tag: "direct", Options: &option.DirectOutboundOptions{}},
			{Type: C.TypeDirect, Tag: "candidate", Options: &option.DirectOutboundOptions{}},
		},
		Services: []option.Service{{
			Type: constant.TypeOutboundEval,
			Tag:  "eval",
			Options: &lboption.OutboundEvalServiceOptions{
				AcquireURL:   server.URL + "/assignments",
				AttestURL:    server.URL + "/attestations",
				SubmitURL:    server.URL + "/reports",
				Token:        "e2e-token",
				CountryCode:  "RU",
				OutboundTag:  "candidate",
				PollInterval: badoption.Duration(10 * time.Millisecond),
			},
		}},
	}

	instance, err := sbox.New(sbox.Options{Context: boxCtx, Options: options})
	require.NoError(t, err, "the service type must be registered by box.BaseContext")

	select {
	case <-api.reported:
		t.Fatal("the service measured before the box started")
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, instance.Start())
	t.Cleanup(func() { require.NoError(t, instance.Close()) })

	select {
	case <-api.reported:
	case <-time.After(30 * time.Second):
		t.Fatal("no report reached the control API")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	assignment := api.assignment()
	require.NotEmpty(t, api.reports)
	report := api.reports[0]
	assert.Equal(t, assignment.ID, report.AssignmentID)
	assert.Equal(t, assignment.ReportToken, report.ReportToken)
	require.Len(t, report.Windows, int(assignment.Sample.WindowsPerExit))
	for _, window := range report.Windows {
		assert.Len(t, window.CandidateAttempts, int(assignment.Sample.AttemptsPerWindow))
		assert.Len(t, window.ControlAttempts, int(assignment.Sample.AttemptsPerWindow))
		assert.Equal(t, "attested-challenge-"+string(rune('0'+window.WindowIndex)), window.AttestationToken)
		for _, attempt := range append(window.CandidateAttempts, window.ControlAttempts...) {
			assert.False(t, attempt.Reachable)
			assert.NotEmpty(t, attempt.FailureCode)
		}
	}
	assert.Equal(t, []string{"challenge-0", "challenge-1"}, api.challenges[:2])
	assert.Equal(t, "Bearer e2e-token", api.tokens[0])
}

// TestOutboundEvalRefusesAConfigWithoutItsArms proves the service names the
// outbound it could not find rather than measuring nothing in silence.
func TestOutboundEvalRefusesAConfigWithoutItsArms(t *testing.T) {
	for name, outbounds := range map[string][]option.Outbound{
		"no control outbound": {
			{Type: C.TypeDirect, Tag: "candidate", Options: &option.DirectOutboundOptions{}},
		},
		"no outbound under test": {
			{Type: C.TypeDirect, Tag: "direct", Options: &option.DirectOutboundOptions{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			instance, err := sbox.New(sbox.Options{
				Context: box.BaseContext(),
				Options: option.Options{
					Log:       &option.LogOptions{Disabled: true},
					Outbounds: outbounds,
					Services: []option.Service{{
						Type: constant.TypeOutboundEval,
						Tag:  "eval",
						Options: &lboption.OutboundEvalServiceOptions{
							AcquireURL:  "https://control.invalid/assignments",
							AttestURL:   "https://control.invalid/attestations",
							SubmitURL:   "https://control.invalid/reports",
							Token:       "token",
							CountryCode: "RU",
							OutboundTag: "candidate",
						},
					}},
				},
			})
			require.NoError(t, err)
			assert.ErrorContains(t, instance.Start(), "outbound evaluation")
			_ = instance.Close()
		})
	}
}

// TestOutboundEvalTokenRotatesThroughTheServiceManager exercises the path an
// embedder uses to replace a credential without restarting the box.
func TestOutboundEvalTokenRotatesThroughTheServiceManager(t *testing.T) {
	api := &controlAPI{reported: make(chan struct{})}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	boxCtx := box.BaseContext()
	instance, err := sbox.New(sbox.Options{
		Context: boxCtx,
		Options: option.Options{
			Log: &option.LogOptions{Disabled: true},
			Outbounds: []option.Outbound{
				{Type: C.TypeDirect, Tag: "direct", Options: &option.DirectOutboundOptions{}},
				{Type: C.TypeDirect, Tag: "candidate", Options: &option.DirectOutboundOptions{}},
			},
			Services: []option.Service{{
				Type: constant.TypeOutboundEval,
				Tag:  "eval",
				Options: &lboption.OutboundEvalServiceOptions{
					AcquireURL:   server.URL + "/assignments",
					AttestURL:    server.URL + "/attestations",
					SubmitURL:    server.URL + "/reports",
					Token:        "first-token",
					CountryCode:  "RU",
					OutboundTag:  "candidate",
					PollInterval: badoption.Duration(10 * time.Millisecond),
				},
			}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	t.Cleanup(func() { require.NoError(t, instance.Close()) })

	// An embedder reaches the service the same way: the box exposes no service
	// accessor, but the context it was built with carries the manager.
	evaluation, found := service.FromContext[A.ServiceManager](boxCtx).Get("eval")
	require.True(t, found)
	setter, ok := evaluation.(lbA.OutboundEvalConfigSetter)
	require.True(t, ok, "the service must be reachable as an OutboundEvalConfigSetter")

	require.NoError(t, setter.SetOutboundEvalConfig(lbA.OutboundEvalConfig{
		Token: "second-token", CountryCode: "RU", OutboundTag: "candidate",
	}))

	require.Eventually(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		for _, token := range api.tokens {
			if token == "Bearer second-token" {
				return true
			}
		}
		return false
	}, 30*time.Second, 20*time.Millisecond)
}
