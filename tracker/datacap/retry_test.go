package datacap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReportFailure_RetriesAndReportsDelta verifies that if a report fails,
// the bytes are added back and reported in the next successful attempt.
func TestReportFailure_RetriesAndReportsDelta(t *testing.T) {
	var reportCount atomic.Int32
	var totalBytesReported atomic.Int64

	// Mock server: Fails 1st request, Succeeds 2nd
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := reportCount.Add(1)

		if count == 1 {
			// First attempt fails
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Subsequent attempts succeed
		var report DataCapReport
		if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
			totalBytesReported.Add(report.BytesUsed)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false, "remainingBytes": 1000, "capLimit": 1000}`))
	}))
	defer server.Close()

	// Short interval for testing
	tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms"}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(make([]byte, 1000))
	ctx := clientcontext.ContextWithClientInfo(context.Background(), clientcontext.ClientInfo{
		IsPro:       false,
		DeviceID:    "device-retry-test",
		Platform:    "test",
		CountryCode: "US",
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	conn, ok := routedConn.(*Conn)
	require.True(t, ok)

	// Simulate Data Usage
	data := make([]byte, 500)
	conn.Read(data)  // 500 bytes received
	conn.Write(data) // 500 bytes sent
	// Total 1000 bytes

	// Wait for 1st report (will fail)
	// Wait for 2nd report (will succeed and should include bytes from 1st)
	// 100ms interval -> wait ~400ms to be safe
	time.Sleep(500 * time.Millisecond)

	conn.Close()

	// Verify
	// Should have at least 2 attempts (1 fail, 1 success)
	assert.GreaterOrEqual(t, reportCount.Load(), int32(2), "Should attempt reporting at least twice")

	// Total bytes reported should be 1000 (succesfully recovered from first failure)
	assert.Equal(t, int64(1000), totalBytesReported.Load(), "Should report total bytes despite initial failure")
}
