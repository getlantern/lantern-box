package outboundeval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeasureAttemptRecordsAReachedTarget(t *testing.T) {
	body := strings.Repeat("x", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	out := &dialOutbound{tag: "candidate", address: server.Listener.Addr().String()}

	attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 0)

	require.NoError(t, attempt.validate())
	assert.True(t, attempt.Reachable)
	assert.Equal(t, http.StatusOK, attempt.HTTPStatus)
	assert.EqualValues(t, len(body), attempt.BytesRead)
	assert.Positive(t, attempt.ThroughputBytesPerSecond)
	assert.Empty(t, attempt.FailureCode)
	assert.GreaterOrEqual(t, attempt.ElapsedMS, attempt.TimeToHeadersMS)
}

func TestMeasureAttemptCountsANonSuccessStatusAgainstTheArm(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusBadGateway,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		out := &dialOutbound{tag: "candidate", address: server.Listener.Addr().String()}

		attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 0)
		server.Close()

		require.NoError(t, attempt.validate())
		assert.False(t, attempt.Reachable, "HTTP %d", status)
		assert.Equal(t, failureHTTPStatus, attempt.FailureCode)
		assert.Equal(t, status, attempt.HTTPStatus)
	}
}

func TestMeasureAttemptStopsAtTheByteCeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
	}))
	t.Cleanup(server.Close)
	out := &dialOutbound{tag: "candidate", address: server.Listener.Addr().String()}

	attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 1024)

	require.NoError(t, attempt.validate())
	assert.True(t, attempt.Reachable)
	assert.EqualValues(t, 1024, attempt.BytesRead)
}

func TestMeasureAttemptReportsAnUnreachableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.Listener.Addr().String()
	server.Close()
	out := &dialOutbound{tag: "candidate", address: address}

	attempt := measureAttempt(context.Background(), out, "https://measure.invalid/x", time.Second, 0)

	require.NoError(t, attempt.validate())
	assert.False(t, attempt.Reachable)
	assert.Equal(t, failureDial, attempt.FailureCode)
}

func TestMeasureAttemptReportsACancelledMeasurement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	out := &dialOutbound{tag: "candidate", address: server.Listener.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempt := measureAttempt(ctx, out, server.URL, time.Second, 0)

	require.NoError(t, attempt.validate())
	assert.Equal(t, failureCanceled, attempt.FailureCode)
}
