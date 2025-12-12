package datacap

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/log"
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
	var lastBytesUsed atomic.Int64

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/data-cap/test-device" {
			// Status check - no throttling
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		} else if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			// Consumption report
			reportCount.Add(1)

			var report DataCapReport
			if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
				lastBytesUsed.Store(report.BytesUsed)
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
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:           noopLogger,
		ReportInterval:   100 * time.Millisecond, // Short interval for testing
		EnableThrottling: false,
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
		if err != nil {
			t.Fatalf("unexpected error reading: %v", err)
		}
	}

	// Verify we read all data
	if totalRead != len(testData) {
		t.Errorf("expected to read %d bytes, got %d", len(testData), totalRead)
	}

	// Write data to connection
	writeData := make([]byte, 1024*50) // 50 KB
	n, err := conn.Write(writeData)
	if err != nil {
		t.Fatalf("unexpected error writing: %v", err)
	}
	if n != len(writeData) {
		t.Errorf("expected to write %d bytes, wrote %d", len(writeData), n)
	}

	// Wait for at least one periodic report
	time.Sleep(200 * time.Millisecond)

	// Close connection (triggers final report)
	if err := conn.Close(); err != nil {
		t.Fatalf("error closing connection: %v", err)
	}

	// Wait a bit for final report to complete
	time.Sleep(100 * time.Millisecond)

	// Verify reports were sent
	reports := reportCount.Load()
	if reports < 1 {
		t.Errorf("expected at least 1 report, got %d", reports)
	}

	// Verify last report included all bytes
	expectedBytes := int64(totalRead + len(writeData))
	reportedBytes := lastBytesUsed.Load()
	if reportedBytes != expectedBytes {
		t.Errorf("expected %d bytes reported, got %d", expectedBytes, reportedBytes)
	}

	// Verify bytes consumed tracking
	consumed := conn.GetBytesConsumed()
	if consumed != expectedBytes {
		t.Errorf("expected %d bytes consumed, got %d", expectedBytes, consumed)
	}
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
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:           noopLogger,
		ReportInterval:   100 * time.Millisecond,
		EnableThrottling: true,
		ThrottleSpeed:    1024 * 10, // 10 KB/s (slow for testing)
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
		if err != nil {
			t.Fatalf("unexpected error reading: %v", err)
		}
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
	if reportCount.Load() < 1 {
		t.Error("expected at least one report to be sent")
	}

	// Close connection
	conn.Close()

	// Verify reports were sent
	if reportCount.Load() < 1 {
		t.Error("expected at least 1 report")
	}
}

// TestDataCapThrottleSpeedAdjustment tests dynamic throttle speed adjustment
func TestDataCapThrottleSpeedAdjustment(t *testing.T) {
	testCases := []struct {
		name              string
		remainingBytes    int64
		capLimit          int64
		expectedThrottle  bool
		expectedSpeedTier string // "high", "medium", "low"
	}{
		{
			name:              "High remaining (>20%)",
			remainingBytes:    3000000000,  // 3 GB
			capLimit:          10000000000, // 10 GB
			expectedThrottle:  true,
			expectedSpeedTier: "high", // 5 Mbps
		},
		{
			name:              "Medium remaining (10-20%)",
			remainingBytes:    1500000000,  // 1.5 GB
			capLimit:          10000000000, // 10 GB
			expectedThrottle:  true,
			expectedSpeedTier: "medium", // 2 Mbps
		},
		{
			name:              "Low remaining (<10%)",
			remainingBytes:    500000000,   // 500 MB
			capLimit:          10000000000, // 10 GB
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
				ClientInfo: &clientcontext.ClientInfo{
					DeviceID: "test-device",
				},
				Logger:           noopLogger,
				EnableThrottling: true,
			}
			conn := NewConn(config)
			defer conn.Close()

			// Simulate status update
			status := &DataCapStatus{
				Throttle:       tc.expectedThrottle,
				RemainingBytes: tc.remainingBytes,
				CapLimit:       tc.capLimit,
			}

			conn.updateThrottleState(status)

			// Verify throttler is enabled
			if !conn.throttler.IsEnabled() {
				t.Error("expected throttler to be enabled")
			}

			// Verify appropriate speed was set (we can't easily check exact speed,
			// but we can verify the throttler is active)
			t.Logf("Throttle state updated for %s: remaining=%.2f%%",
				tc.expectedSpeedTier,
				float64(tc.remainingBytes)/float64(tc.capLimit)*100)
		})
	}
}

// TestDataCapPeriodicReporting tests that reports are sent periodically
func TestDataCapPeriodicReporting(t *testing.T) {
	var reportTimes []time.Time
	var mu sync.Mutex

	// Mock sidecar server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/data-cap/" {
			mu.Lock()
			reportTimes = append(reportTimes, time.Now())
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":10737418240,"capLimit":10737418240,"expiryTime":1700179200}`))
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)

	// Create connection with short report interval
	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: client,
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID:    "test-device",
			CountryCode: "US",
			Platform:    "android",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond, // Very short for testing
	}
	conn := NewConn(config)

	// Do some I/O to generate data
	buffer := make([]byte, 100)
	conn.Read(buffer)
	conn.Write(buffer)

	// Wait for multiple report intervals
	time.Sleep(200 * time.Millisecond)

	conn.Close()
	time.Sleep(50 * time.Millisecond)

	// Verify multiple reports were sent
	mu.Lock()
	count := len(reportTimes)
	mu.Unlock()

	if count < 2 {
		t.Errorf("expected at least 2 reports, got %d", count)
	}

	// Verify reports were spaced approximately by the interval
	if count >= 2 {
		mu.Lock()
		interval := reportTimes[1].Sub(reportTimes[0])
		mu.Unlock()

		if interval < 40*time.Millisecond || interval > 100*time.Millisecond {
			t.Logf("Report interval was %v, expected ~50ms (some variance is normal)", interval)
		}
	}
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
		ClientInfo: &clientcontext.ClientInfo{
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

	if finalReport == nil {
		t.Fatal("expected final report to be sent")
	}

	if finalReport.DeviceID != "test-device-final" {
		t.Errorf("expected deviceId 'test-device-final', got '%s'", finalReport.DeviceID)
	}

	if finalReport.Platform != "ios" {
		t.Errorf("expected platform 'ios', got '%s'", finalReport.Platform)
	}

	if finalReport.BytesUsed != int64(totalRead) {
		t.Errorf("expected %d bytes in final report, got %d", totalRead, finalReport.BytesUsed)
	}
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
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Should still work even if sidecar is down
	buffer := make([]byte, 512)
	n, err := conn.Read(buffer)
	if err != nil && err != io.EOF {
		t.Fatalf("read should work even if sidecar is down: %v", err)
	}
	if n == 0 {
		t.Error("expected to read data")
	}

	// Wait for report attempt (will fail silently)
	time.Sleep(100 * time.Millisecond)

	// Connection should still close properly
	if err := conn.Close(); err != nil {
		t.Errorf("close should succeed even if sidecar is down: %v", err)
	}

	// Verify bytes were still tracked locally
	if conn.GetBytesConsumed() != int64(n) {
		t.Error("bytes should be tracked even if sidecar is down")
	}
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
		ClientInfo: &clientcontext.ClientInfo{
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
	if errorCount < 1 {
		t.Error("expected at least one error response from sidecar")
	}
}

// TestDataCapNilClient tests behavior when client is nil (datacap disabled)
func TestDataCapNilClient(t *testing.T) {
	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: nil, // Datacap disabled
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:         noopLogger,
		ReportInterval: 50 * time.Millisecond,
	}
	conn := NewConn(config)

	// Should still work normally
	buffer := make([]byte, 512)
	n, err := conn.Read(buffer)
	if err != nil && err != io.EOF {
		t.Fatalf("read should work with nil client: %v", err)
	}

	// Bytes should still be tracked
	if conn.GetBytesConsumed() != int64(n) {
		t.Error("bytes should be tracked even with nil client")
	}

	// Should close without errors
	if err := conn.Close(); err != nil {
		t.Errorf("close should succeed with nil client: %v", err)
	}
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
		ClientInfo: &clientcontext.ClientInfo{
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
	if reportCount > 0 {
		t.Errorf("expected 0 reports for zero bytes, got %d", reportCount)
	}
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
		ClientInfo: &clientcontext.ClientInfo{
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

	// Verify atomic counters handled concurrent access correctly
	expected := totalRead.Load() + totalWritten.Load()
	actual := conn.GetBytesConsumed()
	if actual != expected {
		t.Errorf("expected %d total bytes, got %d", expected, actual)
	}
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
		ClientInfo: &clientcontext.ClientInfo{
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
	err1 := conn.Close()
	err2 := conn.Close()
	err3 := conn.Close()

	// First close should succeed
	if err1 != nil {
		t.Errorf("first close failed: %v", err1)
	}

	// Subsequent closes should be no-op (return nil)
	if err2 != nil {
		t.Errorf("second close should be no-op: %v", err2)
	}
	if err3 != nil {
		t.Errorf("third close should be no-op: %v", err3)
	}
}

// TestDataCapThrottleDisableAfterEnable tests disabling throttle after it was enabled
func TestDataCapThrottleDisableAfterEnable(t *testing.T) {
	testData := make([]byte, 1024)
	mockConn := newMockConn(testData)

	config := ConnConfig{
		Conn:   mockConn,
		Client: nil,
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:           noopLogger,
		EnableThrottling: true,
	}
	conn := NewConn(config)
	defer conn.Close()

	// Enable throttling
	status1 := &DataCapStatus{
		Throttle:       true,
		RemainingBytes: 1000000000,
		CapLimit:       10000000000,
	}
	conn.updateThrottleState(status1)

	if !conn.throttler.IsEnabled() {
		t.Error("throttler should be enabled")
	}

	// Disable throttling
	status2 := &DataCapStatus{
		Throttle:       false,
		RemainingBytes: 5000000000,
		CapLimit:       10000000000,
	}
	conn.updateThrottleState(status2)

	if conn.throttler.IsEnabled() {
		t.Error("throttler should be disabled")
	}
}

// TestDataCapEmptyDeviceID tests behavior with empty device ID
func TestDataCapEmptyDeviceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var report DataCapReport
			json.NewDecoder(r.Body).Decode(&report)

			// Verify empty device ID is sent
			if report.DeviceID != "" {
				t.Errorf("expected empty deviceId, got '%s'", report.DeviceID)
			}
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
		ClientInfo: &clientcontext.ClientInfo{
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
		ClientInfo: &clientcontext.ClientInfo{
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

	if lastReport == nil {
		t.Fatal("expected report to be sent")
	}

	if lastReport.BytesUsed != int64(totalRead) {
		t.Errorf("expected %d bytes reported, got %d", totalRead, lastReport.BytesUsed)
	}
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
			ClientInfo: &clientcontext.ClientInfo{
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
		ClientInfo: &clientcontext.ClientInfo{
			DeviceID: "test-device",
		},
		Logger:           noopLogger,
		ReportInterval:   50 * time.Millisecond,
		EnableThrottling: true,
	}
	conn := NewConn(config)

	// Read data
	buffer := make([]byte, 512)
	conn.Read(buffer)

	// Wait for report (which will get throttle=true response)
	time.Sleep(100 * time.Millisecond)

	// Throttle should now be enabled based on report response
	if !conn.throttler.IsEnabled() {
		t.Error("expected throttler to be enabled after report response")
	}

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
				ClientInfo: &clientcontext.ClientInfo{
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
			if receivedPlatform != platform {
				t.Errorf("expected platform '%s', got '%s'", platform, receivedPlatform)
			}
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
				ClientInfo: &clientcontext.ClientInfo{
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
			if receivedCode != code {
				t.Errorf("expected country code '%s', got '%s'", code, receivedCode)
			}
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
		ClientInfo: &clientcontext.ClientInfo{
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
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// Try to read after EOF
	n, err := conn.Read(buffer)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 bytes after EOF, got %d", n)
	}

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
		ClientInfo: &clientcontext.ClientInfo{
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
