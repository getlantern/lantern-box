package outboundeval

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"
	sjson "github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireReportsThatThereIsNothingToMeasure(t *testing.T) {
	s := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := s.api.acquire("token", AssignmentRequest{})

	assert.ErrorIs(t, err, ErrNoAssignment)
}

func TestAcquireCarriesTheBearerTokenAndRequest(t *testing.T) {
	var (
		authorization string
		request       AssignmentRequest
	)
	s := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		writeAssignment(t, w, serverAssignment())
	})

	assignment, err := s.api.acquire("token",
		AssignmentRequest{CountryCode: "RU", IdempotencyKey: "acquire-key", ExitCount: clientExitCount})

	require.NoError(t, err)
	assert.Equal(t, "Bearer token", authorization)
	assert.Equal(t, AssignmentRequest{CountryCode: "RU", IdempotencyKey: "acquire-key", ExitCount: 1}, request)
	assert.Equal(t, "assignment-1", assignment.ID)
	assert.Len(t, assignment.Challenges, 2)
}

func TestAcquireDecodesAssignmentResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/assignment.json")
	require.NoError(t, err)
	s := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(body)
		assert.NoError(t, err)
	})

	assignment, err := s.api.acquire("token", AssignmentRequest{
		CountryCode: "RU", IdempotencyKey: "acquire-key", ExitCount: 1,
	})

	require.NoError(t, err)
	assert.Equal(t, "01JB8Q2P9K3V4W5X6Y7Z8A9B0C", assignment.ID)
	assert.Equal(t, "5vJt0nQf3kZ2xH9cR1bW7yT4", assignment.ReportToken)
	assert.Empty(t, assignment.MeasurementURL)
	assert.Equal(t, fixedNow.Add(15*time.Minute), assignment.ExpiresAt)
	assert.Equal(t, SampleSpec{
		WindowsPerExit: 2, AttemptsPerWindow: 2,
		WindowDurationSeconds: 45, FreshSessionDelayMS: 250,
	}, assignment.Sample)
	assert.Equal(t, []WindowChallenge{
		{WindowIndex: 0, Challenge: "1789257600.hQ2mS8vTzXc1pL0aBd4eFg"},
		{WindowIndex: 1, Challenge: "1789257600.kR7nJ3wYuMb5qN9cVe2tHi"},
	}, assignment.Challenges)
	assert.Equal(t, C.TypeShadowsocks, assignment.Candidate.Options.(O.Outbound).Type)
	assert.Equal(t, C.TypeWireGuard, assignment.Control.Options.(O.Endpoint).Type)
}

func TestAcquireDecodesTheTargets(t *testing.T) {
	s := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`{
			"candidate_target":{"type":"outbound","options":{"type":"dual","tag":"candidate","server":"a"}},
			"control_target":{"type":"endpoint","options":{"type":"dual","tag":"control","peer":"b"}}
		}`))
		assert.NoError(t, err)
	})

	assignment, err := s.api.acquire("token", AssignmentRequest{})

	require.NoError(t, err)
	assert.Equal(t, &EvaluationTarget{Type: EvaluationTargetOutbound, Options: O.Outbound{
		Type: "dual", Tag: "candidate", Options: &dualOutboundOptions{Server: "a"},
	}}, assignment.Candidate)
	assert.Equal(t, &EvaluationTarget{Type: EvaluationTargetEndpoint, Options: O.Endpoint{
		Type: "dual", Tag: "control", Options: &dualEndpointOptions{Peer: "b"},
	}}, assignment.Control)
}

// The cycle does not retry a contract fault, so the sentinel has to survive the
// decoder and the request's own wrapping.
func TestAcquireRefusesATargetItCannotDecode(t *testing.T) {
	for name, body := range map[string]string{
		"unknown type": `{"candidate_target":{"type":"inbound","options":{"type":"dual"}}}`,
		"numeric type": `{"candidate_target":{"type":123,"options":{"type":"dual"}}}`,
		"array":        `{"candidate_target":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(body))
				assert.NoError(t, err)
			})

			_, err := s.api.acquire("token", AssignmentRequest{})

			require.ErrorIs(t, err, ErrInvalidContract)
			assert.False(t, retryableCycleError(err))
		})
	}
}

func TestAttestCarriesNoBearerCredential(t *testing.T) {
	var authorization string
	s := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, err := w.Write([]byte(`{"attestation_token":"attestation","expires_at":"2026-09-17T12:15:00Z"}`))
		assert.NoError(t, err)
	})

	attestation, err := s.api.attest(context.Background(), AttestationRequest{})

	require.NoError(t, err)
	assert.Empty(t, authorization)
	assert.Equal(t, "attestation", attestation.Token)
}

func TestSubmitCarriesTheBearerTokenAndReport(t *testing.T) {
	report := Report{
		ReportToken:    "report-token",
		IdempotencyKey: "report-key",
		Windows: []WindowReport{{
			AttestationToken:  "attestation",
			CandidateAttempts: []Attempt{{Reachable: true, HTTPStatus: http.StatusOK}},
			ControlAttempts:   []Attempt{{FailureCode: failureTimeout}},
			ObservedAt:        fixedNow,
		}},
	}
	s := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer runner-token", r.Header.Get("Authorization"))
		var payload map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Len(t, payload, 3)
		var windows []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(payload["windows"], &windows))
		require.Len(t, windows, 1)
		require.Len(t, windows[0], 4)
		for _, field := range []string{
			"exit_attestation_token", "candidate_attempts", "control_attempts", "observed_at",
		} {
			assert.Contains(t, windows[0], field)
		}
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		var received Report
		require.NoError(t, json.Unmarshal(encoded, &received))
		assert.Equal(t, report, received)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})

	assert.NoError(t, s.api.submit("runner-token", report))
}

func TestSubmitReportsHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})

			assert.ErrorIs(t, s.api.submit("token", Report{}), apiError{status: status})
		})
	}
}

func TestAPIErrorRetryability(t *testing.T) {
	for status, retryable := range map[int]bool{
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusBadRequest:          false,
		http.StatusConflict:            false,
	} {
		assert.Equal(t, retryable, apiError{status: status}.retryable(), "HTTP %d", status)
	}
}

// writeAssignment answers as the control API does; the standard library cannot
// encode the targets' sing-box options.
func writeAssignment(t *testing.T, w http.ResponseWriter, assignment Assignment) {
	t.Helper()
	encoded, err := sjson.MarshalContext(targetContext(context.Background()), &assignment)
	require.NoError(t, err)
	_, err = w.Write(encoded)
	require.NoError(t, err)
}

// serverAssignment is what a control API hands out, current as of this moment
// so it passes the runner's own expiry check.
func serverAssignment() Assignment {
	now := time.Now().UTC().Truncate(time.Second)
	assignment := validAssignment()
	assignment.ExpiresAt = now.Add(15 * time.Minute)
	return assignment
}
