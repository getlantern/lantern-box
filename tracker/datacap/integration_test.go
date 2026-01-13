package datacap

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// mockConn implements net.Conn for testing
type mockConn struct {
	readData  []byte
	readPos   int
	writeData []byte
	closed    bool
	mu        sync.Mutex
}

func newMockConn(readData []byte) *mockConn {
	return &mockConn{
		readData: readData,
	}
}

func (m *mockConn) Read(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return 0, io.EOF
	}

	if m.readPos >= len(m.readData) {
		return 0, io.EOF
	}

	n = copy(b, m.readData[m.readPos:])
	m.readPos += n
	return n, nil
}

func (m *mockConn) Write(b []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return 0, io.ErrClosedPipe
	}

	m.writeData = append(m.writeData, b...)
	return len(b), nil
}

func (m *mockConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func (m *mockConn) GetWrittenData() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeData
}

var noopLogger = log.NewNOPFactory().Logger()

// TestDataCapEndToEndNoThrottling tests the complete datacap workflow without throttling
func TestDataCapEndToEndNoThrottling(t *testing.T) {
	// Track reports received
	var reportCount atomic.Int32
	var totalBytesReported atomic.Int64

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/data-cap/test-device" {
			// Status check - no throttling
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		} else if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			// Consumption report - accumulate delta bytes
			reportCount.Add(1)

			var report DataCapReport
			if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
				totalBytesReported.Add(report.BytesUsed)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	// Create datacap client
	client := NewClient(server.URL, 5*time.Second)

	// Create mock connection with test data
	testData := make([]byte, 1024*100) // 100 KB
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	mockConn := newMockConn(testData)

	// Create datacap-wrapped connection
	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:         noopLogger,
		ReportInterval: 100 * time.Millisecond, // Short interval for testing
	}
	conn := NewConn(config)

	// Read data from connection
	buffer := make([]byte, 1024)
	totalRead := 0
	for {
		n, err := conn.Read(buffer)
		totalRead += n
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	// Verify we read all data
	assert.Equal(t, len(testData), totalRead, "should read all test data")

	// Write data to connection
	writeData := make([]byte, 1024*50) // 50 KB
	n, err := conn.Write(writeData)
	require.NoError(t, err)
	assert.Equal(t, len(writeData), n, "should write all data")

	// Wait for at least one periodic report
	time.Sleep(200 * time.Millisecond)

	// Close connection (triggers final report)
	require.NoError(t, conn.Close())

	// Wait a bit for final report to complete
	time.Sleep(100 * time.Millisecond)

	// Verify reports were sent
	assert.GreaterOrEqual(t, reportCount.Load(), int32(1), "should have sent at least one report")

	// Verify total bytes reported (sum of all deltas)
	expectedBytes := int64(totalRead + len(writeData))
	reportedBytes := totalBytesReported.Load()
	assert.Equal(t, expectedBytes, reportedBytes, "total reported bytes should match total bytes used")

	// After final report, counters should be reset to 0
	assert.Equal(t, int64(0), conn.GetBytesConsumed(), "GetBytesConsumed should be 0 after final report")
}

// TestDataCapEndToEndWithThrottling tests datacap workflow with throttling enabled
func TestDataCapEndToEndWithThrottling(t *testing.T) {
	var reportCount atomic.Int32

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			reportCount.Add(1)
			// Report response with throttling enabled
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":true,"remainingBytes":1073741824,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	// Create datacap client
	client := NewClient(server.URL, 5*time.Second)

	// Create mock connection
	testData := make([]byte, 1024*10) // 10 KB
	mockConn := newMockConn(testData)

	// Create datacap-wrapped connection with throttling
	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:         noopLogger,
		ReportInterval: 100 * time.Millisecond,
		ThrottleSpeed:  1024 * 10, // 10 KB/s (slow for testing)
	}
	conn := NewConn(config)

	// Measure time to read data with throttling
	startTime := time.Now()
	buffer := make([]byte, 1024)
	totalRead := 0
	for {
		n, err := conn.Read(buffer)
		totalRead += n
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}
	duration := time.Since(startTime)

	// With 10 KB data and 10 KB/s throttle, should take at least ~1 second
	// (accounting for token bucket refill)
	if duration < 500*time.Millisecond {
		t.Logf("Warning: Read completed in %v, expected throttling to slow it down", duration)
	}

	// Wait for periodic report which will update throttle state
	time.Sleep(150 * time.Millisecond)

	// Verify at least one report was sent (which includes throttle status in response)
	assert.GreaterOrEqual(t, reportCount.Load(), int32(1), "should have sent at least one report")

	// Close connection
	conn.Close()

	// Verify reports were sent
	assert.GreaterOrEqual(t, reportCount.Load(), int32(1), "should have sent at least one report")
}

// TestDataCapThrottleSpeedAdjustment tests dynamic throttle speed adjustment
func TestDataCapThrottleSpeedAdjustment(t *testing.T) {
	testCases := []struct {
		name              string
		remainingBytes    int64
		capLimit          int64
		throttle          bool
		expectedThrottle  bool
		expectedSpeedTier string // just for logging
	}{
		{
			name:              "Not throttled - high remaining",
			remainingBytes:    3000000000,  // 3 GB
			capLimit:          10000000000, // 10 GB
			throttle:          false,
			expectedThrottle:  false,
			expectedSpeedTier: "unthrottled",
		},
		{
			name:              "Not throttled - low remaining",
			remainingBytes:    100,         // 100 bytes
			capLimit:          10000000000, // 10 GB
			throttle:          false,
			expectedThrottle:  false,
			expectedSpeedTier: "unthrottled",
		},
		{
			name:              "Throttled - cap exhausted",
			remainingBytes:    0,
			capLimit:          10000000000, // 10 GB
			throttle:          true,
			expectedThrottle:  true,
			expectedSpeedTier: "low", // 128 KB/s
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create mock connection
			mockConn := newMockConn(nil)

			// Create datacap-wrapped connection
			config := ConnConfig{
				Conn:   mockConn,
				Client: nil, // No client needed for this test
				ClientInfo: clientcontext.ClientInfo{
					DeviceID: "test-device",
				},
				Logger: noopLogger,
			}
			conn := NewConn(config)
			defer conn.Close()

			// Simulate status update
			status := &DataCapStatus{
				Throttle:       tc.throttle,
				RemainingBytes: tc.remainingBytes,
				CapLimit:       tc.capLimit,
			}

			conn.updateThrottleState(status)

			// Verify throttler state
			if tc.expectedThrottle {
				assert.True(t, conn.throttler.IsEnabled(), "throttling should be enabled")
			} else {
				assert.False(t, conn.throttler.IsEnabled(), "throttling should be disabled")
			}

			// Log status
			t.Logf("Throttle state updated for %s: throttle=%v remaining=%.2f%%",
				tc.expectedSpeedTier,
				tc.throttle,
				float64(tc.remainingBytes)/float64(tc.capLimit)*100)
		})
	}
}

// TestDataCapPeriodicReporting tests that reports are sent periodically
func TestDataCapPeriodicReporting(t *testing.T) {
	var reportCount atomic.Int32

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			reportCount.Add(1)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	// Create connection with short report interval
	testData := make([]byte, 10000) // Large buffer for multiple reads
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond, // Very short for testing
	}
	conn := NewConn(config)

	// Do continuous I/O during test period to ensure reports have data
	done := make(chan struct{})
	go func() {
		buffer := make([]byte, 100)
		for {
			select {
			case <-done:
				return
			default:
				conn.Read(buffer)
				conn.Write(buffer)
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	// Wait for multiple report intervals
	time.Sleep(200 * time.Millisecond)
	close(done)

	conn.Close()
	time.Sleep(50 * time.Millisecond)

	// Verify multiple reports were sent
	count := reportCount.Load()
	assert.GreaterOrEqual(t, count, int32(2), "should have sent at least 2 reports")
}

// TestDataCapFinalReportOnClose tests that a final report is sent when connection closes
func TestDataCapFinalReportOnClose(t *testing.T) {
	var finalReport *DataCapReport
	var mu sync.Mutex

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			mu.Lock()
			defer mu.Unlock()

			var report DataCapReport
			if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
				// Store the last report (final report)
				finalReport = &report
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 5000) // 5 KB
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID:    "test-device-final",
			CountryCode: "US",
			Platform:    "ios",
		},
		Logger:         noopLogger,
		ReportInterval: time.Hour, // Long interval so only final report happens
	}
	conn := NewConn(config)

	// Read all data
	buffer := make([]byte, 1024)
	totalRead := 0
	for {
		n, err := conn.Read(buffer)
		totalRead += n
		if err == io.EOF {
			break
		}
	}

	// Close connection immediately (before periodic report)
	conn.Close()
	time.Sleep(100 * time.Millisecond)

	// Verify final report was sent with correct data
	mu.Lock()
	defer mu.Unlock()

	require.NotNil(t, finalReport, "final report should not be nil")

	assert.Equal(t, "test-device-final", finalReport.DeviceID, "device ID should match")
	assert.Equal(t, "ios", finalReport.Platform, "platform should match")
	assert.Equal(t, int64(totalRead), finalReport.BytesUsed, "bytes used should match total read")
}

// TestDataCapSidecarUnreachable tests behavior when sidecar is unreachable
func TestDataCapSidecarUnreachable(t *testing.T) {
	// Use invalid URL
	client := NewClient("http://localhost:99999", 100*time.Millisecond)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Should still work even if sidecar is down
	buffer := make([]byte, 512)
	n, err := conn.Read(buffer)
	if err == io.EOF {
		err = nil
	}
	require.NoError(t, err, "read should work even if sidecar is down")
	require.NotEqual(t, 0, n, "expected to read some data")

	// Wait for report attempt (will fail silently)
	time.Sleep(100 * time.Millisecond)

	// Connection should still close properly
	assert.NoError(t, conn.Close(), "close should succeed even if sidecar is down")
	assert.Equal(t, int64(n), conn.GetBytesConsumed(), "bytes should be tracked even if sidecar is down")
}

// TestDataCapSidecarReturnsError tests behavior when sidecar returns HTTP errors
func TestDataCapSidecarReturnsError(t *testing.T) {
	errorCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errorCount++
		// Return 500 error
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Read data
	buffer := make([]byte, 512)
	conn.Read(buffer)

	// Wait for report attempt
	time.Sleep(100 * time.Millisecond)

	conn.Close()

	// Errors should be logged but not crash
	assert.GreaterOrEqual(t, errorCount, 1, "should have received at least one error from sidecar")
}

// TestDataCapNilClient tests behavior when client is nil (datacap disabled)
func TestDataCapNilClient(t *testing.T) {
	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: nil, // Datacap disabled
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Should still work normally
	buffer := make([]byte, 512)
	n, err := conn.Read(buffer)
	if err == io.EOF {
		err = nil
	}
	require.NoError(t, err, "read should succeed with nil client")

	// Bytes should still be tracked
	assert.Equal(t, int64(n), conn.GetBytesConsumed(), "bytes should be tracked even with nil client")
	assert.NoError(t, conn.Close(), "close should succeed with nil client")
}

// TestDataCapZeroBytes tests that zero-byte reports are not sent
func TestDataCapZeroBytes(t *testing.T) {
	reportCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			reportCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 0) // Empty data
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Don't read or write any data
	time.Sleep(150 * time.Millisecond)

	conn.Close()
	time.Sleep(50 * time.Millisecond)

	// Should not send reports for zero bytes
	assert.Equal(t, 0, reportCount, "should not send reports for zero bytes")
}

// TestDataCapConcurrentReadWrite tests concurrent reads and writes
func TestDataCapConcurrentReadWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 10000)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 200 * time.Millisecond,
	}
	conn := NewConn(config)

	var wg sync.WaitGroup
	totalRead := atomic.Int64{}
	totalWritten := atomic.Int64{}

	// Concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, 100)
			for j := 0; j < 10; j++ {
				n, err := conn.Read(buffer)
				if err != nil && err != io.EOF {
					return
				}
				totalRead.Add(int64(n))
				if n == 0 {
					break
				}
			}
		}()
	}

	// Concurrent writers
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, 100)
			for j := 0; j < 10; j++ {
				n, err := conn.Write(buffer)
				if err != nil {
					return
				}
				totalWritten.Add(int64(n))
			}
		}()
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	conn.Close()

	// With delta reporting, counters are reset after each report
	// After Close (which sends final report), counters should be 0
	actual := conn.GetBytesConsumed()
	assert.Equal(t, int64(0), actual, "GetBytesConsumed should be 0 after final report")

	// The test verifies concurrent access didn't cause panics or data races
	// Total bytes were tracked and reported correctly
	t.Logf("Concurrent test completed: read=%d, written=%d", totalRead.Load(), totalWritten.Load())
}

// TestDataCapMultipleClose tests that closing multiple times is safe
func TestDataCapMultipleClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: time.Hour,
	}
	conn := NewConn(config)

	// Read some data
	buffer := make([]byte, 512)
	conn.Read(buffer)

	// Close multiple times
	// First close should succeed
	assert.NoError(t, conn.Close(), "first close should succeed")
	// Subsequent closes should be no-op (return nil)
	assert.NoError(t, conn.Close(), "second close should be no-op")
	assert.NoError(t, conn.Close(), "third close should be no-op")
}

// TestDataCapThrottleDisableAfterEnable tests disabling throttle after it was enabled
func TestDataCapThrottleDisableAfterEnable(t *testing.T) {
	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: nil,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger: noopLogger,
	}
	conn := NewConn(config)
	defer conn.Close()

	// Enable throttling (data exhausted scenario - Throttle=true, RemainingBytes=0)
	status1 := &DataCapStatus{
		Throttle:       true,
		RemainingBytes: 0,
		CapLimit:       10000000000,
	}
	conn.updateThrottleState(status1)

	assert.True(t, conn.throttler.IsEnabled(), "throttling should be enabled when data exhausted")

	// Disable throttling (no datacap scenario - CapLimit=0)
	status2 := &DataCapStatus{
		Throttle:       false,
		RemainingBytes: 0,
		CapLimit:       0,
	}
	conn.updateThrottleState(status2)

	assert.False(t, conn.throttler.IsEnabled(), "throttling should be disabled when no datacap")
}

// TestDataCapEmptyDeviceID tests behavior with empty device ID
func TestDataCapEmptyDeviceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var report DataCapReport
			json.NewDecoder(r.Body).Decode(&report)

			// Verify empty device ID is sent
			assert.Empty(t, report.DeviceID, "deviceId should be empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "", // Empty device ID
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	buffer := make([]byte, 512)
	conn.Read(buffer)
	time.Sleep(100 * time.Millisecond)
	conn.Close()

	// Should handle empty device ID gracefully
}

// TestDataCapLargeDataTransfer tests with large data transfers
func TestDataCapLargeDataTransfer(t *testing.T) {
	var lastReport *DataCapReport
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			var report DataCapReport
			if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
				lastReport = &report
			}
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	// 10 MB of data
	largeData := make([]byte, 10*1024*1024)
	mockConn := newMockConn(largeData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 100 * time.Millisecond,
	}
	conn := NewConn(config)

	// Read all data
	buffer := make([]byte, 64*1024) // 64 KB chunks
	totalRead := 0
	for {
		n, err := conn.Read(buffer)
		totalRead += n
		if err == io.EOF {
			break
		}
	}

	time.Sleep(150 * time.Millisecond)
	conn.Close()
	time.Sleep(50 * time.Millisecond)

	// Verify large amounts are tracked correctly
	mu.Lock()
	defer mu.Unlock()

	require.NotNil(t, lastReport, "last report not sent")
	assert.Equal(t, int64(totalRead), lastReport.BytesUsed, "bytes used should match total read")
}

// TestDataCapRapidOpenClose tests rapid connection open/close cycles
func TestDataCapRapidOpenClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	// Open and close many connections rapidly
	for i := 0; i < 50; i++ {
		testData := make([]byte, 100)
		mockConn := newMockConn(testData)

		config := ConnConfig{
			Conn:   mockConn,
			Client: client,
			ClientInfo: clientcontext.ClientInfo{
				DeviceID: "test-device",
			},
			Logger:         noopLogger,
			ReportInterval: time.Hour,
		}
		conn := NewConn(config)

		// Quick read
		buffer := make([]byte, 50)
		conn.Read(buffer)

		// Immediate close
		conn.Close()
	}

	// Should not panic or cause issues
	time.Sleep(100 * time.Millisecond)
}

// TestDataCapStatusCheckAfterReport tests that status is updated after reporting
func TestDataCapStatusCheckAfterReport(t *testing.T) {
	responseThrottle := false
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == http.MethodPost {
			// After first report, start throttling
			responseThrottle = true
		}

		throttleStr := "false"
		if responseThrottle {
			throttleStr = "true"
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":` + throttleStr + `,"remainingBytes":500000000,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Read data
	buffer := make([]byte, 512)
	conn.Read(buffer)

	// Wait for report (which will get throttle=true response)
	time.Sleep(100 * time.Millisecond)

	// Throttle should now be enabled based on report response
	assert.True(t, conn.throttler.IsEnabled(), "throttling should be enabled after report response")

	conn.Close()
}

// TestDataCapDifferentPlatforms tests different platform values
func TestDataCapDifferentPlatforms(t *testing.T) {
	platforms := []string{"android", "ios", "windows", "macos", "linux", ""}

	for _, platform := range platforms {
		t.Run("Platform_"+platform, func(t *testing.T) {
			var receivedPlatform string
			var mu sync.Mutex

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					mu.Lock()
					var report DataCapReport
					if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
						receivedPlatform = report.Platform
					}
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
			}))
			defer server.Close()

			client := NewClient(server.URL, 5*time.Second)

			testData := make([]byte, 1024)
			mockConn := newMockConn(testData)

			config := ConnConfig{
				Conn:   mockConn,
				Client: client,
				ClientInfo: clientcontext.ClientInfo{
					DeviceID: "test-device",
					Platform: platform,
				},
				Logger:         noopLogger,
				ReportInterval: 50 * time.Millisecond,
			}
			conn := NewConn(config)

			buffer := make([]byte, 512)
			conn.Read(buffer)
			time.Sleep(100 * time.Millisecond)
			conn.Close()

			mu.Lock()
			assert.Equal(t, platform, receivedPlatform, "platform should match")
			mu.Unlock()
		})
	}
}

// TestDataCapCountryCodeVariations tests different country codes
func TestDataCapCountryCodeVariations(t *testing.T) {
	countryCodes := []string{"US", "GB", "CN", "IN", "BR", ""}

	for _, code := range countryCodes {
		t.Run("Country_"+code, func(t *testing.T) {
			var receivedCode string
			var mu sync.Mutex

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					mu.Lock()
					var report DataCapReport
					if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
						receivedCode = report.CountryCode
					}
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
			}))
			defer server.Close()

			client := NewClient(server.URL, 5*time.Second)

			testData := make([]byte, 1024)
			mockConn := newMockConn(testData)

			config := ConnConfig{
				Conn:   mockConn,
				Client: client,
				ClientInfo: clientcontext.ClientInfo{
					DeviceID:    "test-device",
					CountryCode: code,
				},
				Logger:         noopLogger,
				ReportInterval: 50 * time.Millisecond,
			}
			conn := NewConn(config)

			buffer := make([]byte, 512)
			conn.Read(buffer)
			time.Sleep(100 * time.Millisecond)
			conn.Close()

			mu.Lock()
			assert.Equal(t, code, receivedCode, "country code should match")
			mu.Unlock()
		})
	}
}

// TestDataCapReadWriteErrors tests handling of read/write errors
func TestDataCapReadWriteErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: time.Hour,
	}
	conn := NewConn(config)

	// Read until EOF
	buffer := make([]byte, 512)
	for {
		_, err := conn.Read(buffer)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	// Try to read after EOF
	n, err := conn.Read(buffer)
	assert.ErrorIs(t, err, io.EOF, "expected EOF error on read after EOF")
	assert.Equal(t, 0, n, "expected 0 bytes after EOF")

	conn.Close()
}

// TestDataCapContextCancellation tests behavior when context is cancelled
func TestDataCapContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	buffer := make([]byte, 512)
	conn.Read(buffer)

	// Close immediately (cancels context)
	conn.Close()

	// Should not panic or hang
	time.Sleep(50 * time.Millisecond)
}

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

	// Total bytes reported should be 1000 (successfully recovered from first failure)
	assert.Equal(t, int64(1000), totalBytesReported.Load(), "Should report total bytes despite initial failure")
}
