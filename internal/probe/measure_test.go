package probe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeasure_RecordsStatusBytesAndTiming(t *testing.T) {
	body := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(srv.Listener.Addr().String(), &conns)}

	result, err := Measure(context.Background(), out, srv.URL, time.Second, 0)

	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, result.HTTPStatus)
	assert.EqualValues(t, len(body), result.BytesRead)
	assert.Positive(t, result.ThroughputBytesPerSecond)
	assert.Positive(t, result.TimeToHeaders)
	assert.GreaterOrEqual(t, result.Elapsed, result.TimeToHeaders)
	require.Len(t, conns, 1)
	assert.True(t, conns[0].closed.Load())
}

func TestMeasure_StopsAtTheByteLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
	}))
	defer srv.Close()
	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(srv.Listener.Addr().String(), &conns)}

	result, err := Measure(context.Background(), out, srv.URL, time.Second, 1024)

	require.NoError(t, err)
	assert.EqualValues(t, 1024, result.BytesRead)
}

func TestMeasure_ReportsTheStageItFailedAt(t *testing.T) {
	const spent = 20 * time.Millisecond
	out := &stubOutbound{dial: func(context.Context) (net.Conn, error) {
		time.Sleep(spent)
		return nil, errors.New("no route")
	}}

	result, err := Measure(context.Background(), out, "http://192.0.2.1/x", time.Second, 0)

	assert.ErrorIs(t, err, ErrDial)
	// A path that never opened still reports how long it took to find that out.
	assert.GreaterOrEqual(t, result.Elapsed, spent)
}

// Run reports a body failure to its caller, along with how long the probe took,
// so the caller decides whether a truncated response counts.
func TestRun_ReportsABodyThatFailedPartwayWithItsDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(strings.Repeat("x", 16)))
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			require.NoError(t, err)
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	var conns []*trackedConn
	out := &stubOutbound{dial: dialerFor(srv.Listener.Addr().String(), &conns)}

	delay, err := Run(context.Background(), out, srv.URL, time.Second)

	assert.ErrorIs(t, err, ErrResponseBody)
	assert.Positive(t, delay)
}
