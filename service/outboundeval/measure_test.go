package outboundeval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMeasureAttemptRecordsAReachedTarget(t *testing.T) {
	body := strings.Repeat("x", 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	out := &stubOutbound{tag: "candidate", address: server.Listener.Addr().String()}

	attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 0)

	assert.True(t, attempt.Reachable)
	assert.Equal(t, http.StatusOK, attempt.HTTPStatus)
	assert.EqualValues(t, len(body), attempt.BytesRead)
	assert.Positive(t, attempt.ThroughputBytesPerSecond)
	assert.Empty(t, attempt.FailureCode)
	// Compared whole, since the elapsed figure is whole milliseconds and the
	// latency one keeps its fraction.
	assert.GreaterOrEqual(t, attempt.ElapsedMS, int64(attempt.TimeToHeadersMS))
}

func TestMeasureAttemptCountsANonSuccessStatusAgainstTheArm(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusBadGateway,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		out := &stubOutbound{tag: "candidate", address: server.Listener.Addr().String()}

		attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 0)
		server.Close()

		assert.False(t, attempt.Reachable, "HTTP %d", status)
		assert.Equal(t, failureHTTPStatus, attempt.FailureCode)
		assert.Equal(t, status, attempt.HTTPStatus)
	}
}

func TestMeasureAttemptReportsABlockPageThatCutsItsBodyShort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(strings.Repeat("x", 16)))
		// Hand the short body to the client before taking the connection, so
		// the measurement reads a truncated body rather than nothing at all.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the test server does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)
	out := &stubOutbound{tag: "candidate", address: server.Listener.Addr().String()}

	attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 0)

	assert.False(t, attempt.Reachable)
	assert.Equal(t, failureHTTPStatus, attempt.FailureCode,
		"the status the target served outranks its body then failing")
	assert.Equal(t, http.StatusForbidden, attempt.HTTPStatus)
}

func TestMeasureAttemptStopsAtTheByteCeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
	}))
	t.Cleanup(server.Close)
	out := &stubOutbound{tag: "candidate", address: server.Listener.Addr().String()}

	attempt := measureAttempt(context.Background(), out, server.URL, time.Second, 1024)

	assert.True(t, attempt.Reachable)
	assert.EqualValues(t, 1024, attempt.BytesRead)
}

func TestMeasureAttemptReportsAnUnreachableTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.Listener.Addr().String()
	server.Close()
	out := &stubOutbound{tag: "candidate", address: address}

	attempt := measureAttempt(context.Background(), out, "https://measure.invalid/x", time.Second, 0)

	assert.False(t, attempt.Reachable)
	assert.Equal(t, failureDial, attempt.FailureCode)
}

func TestMeasureAttemptReportsACancelledMeasurement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	out := &stubOutbound{tag: "candidate", address: server.Listener.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempt := measureAttempt(ctx, out, server.URL, time.Second, 0)

	assert.Equal(t, failureCanceled, attempt.FailureCode)
}
