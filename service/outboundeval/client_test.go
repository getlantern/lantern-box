package outboundeval

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireReportsThatThereIsNothingToMeasure(t *testing.T) {
	s, _ := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := s.api.acquire(context.Background(), s.options.AcquireURL, "token", AssignmentRequest{})

	assert.ErrorIs(t, err, ErrNoAssignment)
}

func TestAcquireCarriesTheBearerTokenAndRequest(t *testing.T) {
	var (
		authorization string
		request       AssignmentRequest
	)
	s, _ := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.NoError(t, json.NewEncoder(w).Encode(serverAssignment()))
	})

	assignment, err := s.api.acquire(context.Background(), s.options.AcquireURL, "token",
		AssignmentRequest{CountryCode: "RU", ExitCount: clientExitCount})

	require.NoError(t, err)
	assert.Equal(t, "Bearer token", authorization)
	assert.Equal(t, AssignmentRequest{CountryCode: "RU", ExitCount: 1}, request)
	assert.Equal(t, "assignment-1", assignment.ID)
	assert.Len(t, assignment.Challenges, 2)
}

func TestAttestCarriesNoBearerCredential(t *testing.T) {
	var authorization string
	s, _ := wiredService(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		require.NoError(t, json.NewEncoder(w).Encode(Attestation{Token: "attestation"}))
	})

	attestation, err := s.api.attest(context.Background(), s.options.AttestURL, AttestationRequest{})

	require.NoError(t, err)
	assert.Empty(t, authorization)
	assert.Equal(t, "attestation", attestation.Token)
}

func TestAttestRejectsAnUnusableToken(t *testing.T) {
	s, _ := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(Attestation{}))
	})

	_, err := s.api.attest(context.Background(), s.options.AttestURL, AttestationRequest{})

	assert.ErrorIs(t, err, ErrInvalidContract)
}

func TestSubmitTreatsAConflictAsAlreadyAccepted(t *testing.T) {
	status := http.StatusConflict
	s, _ := wiredService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})

	assert.NoError(t, s.api.submit(context.Background(), s.options.SubmitURL, Report{}))

	status = http.StatusInternalServerError
	assert.Error(t, s.api.submit(context.Background(), s.options.SubmitURL, Report{}))
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

// serverAssignment is what a control API hands out, current as of this moment
// so it passes the runner's own expiry check.
func serverAssignment() Assignment {
	now := time.Now().UTC().Truncate(time.Second)
	assignment := validAssignment()
	assignment.ServerTime = now
	assignment.ExpiresAt = now.Add(15 * time.Minute)
	return assignment
}
