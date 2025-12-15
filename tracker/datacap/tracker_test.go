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

// Scenario 3: Datacap URL present & Free Client & No Cap (Throttle: false)
func TestRoutedConnection_FreeUserNoCap_DisablesThrottling(t *testing.T) {
	// Mock server returning Throttle: false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"throttle":false, "remainingBytes": 1000, "capLimit": 1000}`))
	}))
	defer server.Close()

	tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms"}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(make([]byte, 1024))
	ctx := service.ContextWithPtr(context.Background(), &clientcontext.ClientInfo{
		IsPro:       false,
		DeviceID:    "device-free-nocap",
		Platform:    "test",
		CountryCode: "US",
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	// Should return wrapped connection
	assert.NotEqual(t, mockConn, routedConn)

	conn := routedConn.(*Conn)
	// Read some data to trigger reporting
	_, _ = conn.Read(make([]byte, 10))

	// Wait for report to happen to update status
	time.Sleep(200 * time.Millisecond)

	// Verify throttler is disabled
	assert.False(t, conn.throttler.IsEnabled(), "Throttler should be disabled for uncapped user")
	conn.Close()
}

// Scenario 4: Datacap URL present & Free Client & With Cap (Throttle: true)
func TestRoutedConnection_FreeUserWithCap_EnablesThrottling(t *testing.T) {
	// Mock server returning Throttle: true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"throttle":true, "remainingBytes": 100, "capLimit": 1000}`))
	}))
	defer server.Close()

	tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms", EnableThrottling: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)

	mockConn := newMockConn(make([]byte, 1024))
	ctx := service.ContextWithPtr(context.Background(), &clientcontext.ClientInfo{
		IsPro:       false,
		DeviceID:    "device-free-capped",
		Platform:    "test",
		CountryCode: "US",
	})

	routedConn := tracker.RoutedConnection(ctx, mockConn, adapter.InboundContext{}, nil, nil)
	// Should return wrapped connection
	assert.NotEqual(t, mockConn, routedConn)

	conn := routedConn.(*Conn)
	// Read some data to trigger reporting
	_, _ = conn.Read(make([]byte, 10))

	// Wait for report to happen to update status
	time.Sleep(200 * time.Millisecond)

	// Verify throttler is enabled
	assert.True(t, conn.throttler.IsEnabled(), "Throttler should be enabled for capped user")
	conn.Close()
}
