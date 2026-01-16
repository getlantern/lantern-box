package datacap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario 1: NewDatacapTracker returns error if URL is empty
func TestNewDatacapTracker_MissingURL_ReturnsError(t *testing.T) {
	_, err := NewDatacapTracker(Options{URL: ""}, log.NewNOPFactory().Logger())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url not defined")
}

// Scenario 2: Datacap URL is present & Client is Pro
func TestRoutedConnection_ProClient_SkipsTracking(t *testing.T) {
	tracker, err := NewDatacapTracker(Options{URL: "http://example.com"}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(nil)
	ctx := service.ContextWithPtr(context.Background(), &clientcontext.ClientInfo{
		IsPro: true,
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	// Should return original connection (skipped)
	assert.Equal(t, mockConn, routedConn)
}

// Scenario 3: Datacap URL present & Free Client & Throttling Disabled
func TestRoutedConnection_FreeUser_ThrottlingDisabled(t *testing.T) {
	// Mock server returning Throttle: false (throttling disabled)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"throttle":false, "capLimit": 1000}`))
	}))
	defer server.Close()

	tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms"}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(make([]byte, 1024))
	ctx := clientcontext.ContextWithClientInfo(context.Background(), clientcontext.ClientInfo{
		IsPro:       false,
		DeviceID:    "device-free-no-throttle",
		Platform:    "test",
		CountryCode: "US",
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	assert.NotEqual(t, mockConn, routedConn)

	conn, ok := routedConn.(*Conn)
	require.True(t, ok, "routedConn should be *Conn")

	_, _ = conn.Read(make([]byte, 10))
	time.Sleep(200 * time.Millisecond)

	// Throttling should be DISABLED
	assert.False(t, conn.throttler.IsEnabled(), "Throttler should be disabled")
	conn.Close()
}

// Scenario 4: Datacap URL present & Free Client & Data Exhausted (Throttle: true)
func TestRoutedConnection_FreeUserWithCap_EnablesThrottling(t *testing.T) {
	// Mock server returning Throttle: true (data exhausted, remainingBytes <= 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"throttle":true, "remainingBytes": 0, "capLimit": 1000}`))
	}))
	defer server.Close()

	tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms"}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(make([]byte, 1024))
	ctx := clientcontext.ContextWithClientInfo(context.Background(), clientcontext.ClientInfo{
		IsPro:       false,
		DeviceID:    "device-free-capped",
		Platform:    "test",
		CountryCode: "US",
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	assert.NotEqual(t, mockConn, routedConn)

	conn, ok := routedConn.(*Conn)
	require.True(t, ok, "routedConn should be *Conn")

	_, _ = conn.Read(make([]byte, 10))
	time.Sleep(200 * time.Millisecond)

	// Throttler should be enabled when Throttle=true (data exhausted)
	assert.True(t, conn.throttler.IsEnabled(), "Throttler should be enabled for capped user")

	// Verify rates: Write (Download) should be throttled, Read (Upload) should allow more
	assert.Equal(t, int64(lowTierSpeedBytesPerSec), conn.throttler.GetWriteRate(), "Write rate (Download) should be throttled to low tier")
	assert.Equal(t, int64(defaultUploadSpeedBytesPerSec), conn.throttler.GetReadRate(), "Read rate (Upload) should be default upload speed")

	conn.Close()
}
