package datacap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDataCapClient(t *testing.T) {
	// Create a test server
	statusCalled := false
	reportCalled := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data-cap/test-device-123" && r.Method == http.MethodGet {
			statusCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":9663676416,"capLimit":10737418240,"expiryTime":1700179200}`))
		} else if r.URL.Path == "/data-cap/" && r.Method == http.MethodPost {
			reportCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"throttle":false,"remainingBytes":9663676416,"capLimit":10737418240,"expiryTime":1700179200}`))
		} else {
			t.Errorf("unexpected path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Create client
	client := NewClient(server.URL, 5*time.Second)

	// Test GetDataCapStatus
	ctx := context.Background()
	status, err := client.GetDataCapStatus(ctx, "test-device-123")
	if err != nil {
		t.Fatalf("GetDataCapStatus failed: %v", err)
	}

	if !statusCalled {
		t.Error("status endpoint was not called")
	}

	if !status.Throttle {
		// Expected: throttle should be false
	} else {
		t.Error("expected throttle=false, got true")
	}

	if status.RemainingBytes != 9663676416 {
		t.Errorf("expected remainingBytes=9663676416, got %d", status.RemainingBytes)
	}

	if status.CapLimit != 10737418240 {
		t.Errorf("expected capLimit=10737418240, got %d", status.CapLimit)
	}

	if status.ExpiryTime != 1700179200 {
		t.Errorf("expected expiryTime=1700179200, got %d", status.ExpiryTime)
	}

	// Test ReportDataCapConsumption
	report := &DataCapReport{
		DeviceID:    "test-device-123",
		CountryCode: "US",
		Platform:    "android",
		BytesUsed:   1048576,
	}

	status, err = client.ReportDataCapConsumption(ctx, report)
	if err != nil {
		t.Fatalf("ReportDataCapConsumption failed: %v", err)
	}

	if !reportCalled {
		t.Error("report endpoint was not called")
	}

	if status == nil {
		t.Fatal("expected status response, got nil")
	}
}

func TestDataCapClientInvalidURL(t *testing.T) {
	client := NewClient("http://invalid-url-that-does-not-exist:9999", 1*time.Second)

	ctx := context.Background()
	_, err := client.GetDataCapStatus(ctx, "test-device")
	if err == nil {
		t.Error("expected error for invalid URL, got nil")
	}
}

func TestDataCapClientTimeout(t *testing.T) {
	// Create a server that sleeps longer than timeout
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client with short timeout
	client := NewClient(server.URL, 100*time.Millisecond)

	ctx := context.Background()
	_, err := client.GetDataCapStatus(ctx, "test-device")
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestDataCapClientThrottleTrue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Device is over cap, should be throttled
		w.Write([]byte(`{"throttle":true,"remainingBytes":0,"capLimit":1073741824,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)
	ctx := context.Background()

	status, err := client.GetDataCapStatus(ctx, "test-device")
	if err != nil {
		t.Fatalf("GetDataCapStatus failed: %v", err)
	}

	if !status.Throttle {
		t.Error("expected throttle=true, got false")
	}

	if status.RemainingBytes != 0 {
		t.Errorf("expected remainingBytes=0, got %d", status.RemainingBytes)
	}
}

func TestDataCapClientAcceptHeader(t *testing.T) {
	acceptHeaderReceived := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Accept header is set
		if r.Header.Get("Accept") == "application/json" {
			acceptHeaderReceived = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":1000000,"capLimit":1073741824,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)
	ctx := context.Background()

	_, err := client.GetDataCapStatus(ctx, "test-device")
	if err != nil {
		t.Fatalf("GetDataCapStatus failed: %v", err)
	}

	if !acceptHeaderReceived {
		t.Error("Accept: application/json header was not sent")
	}
}

func TestDataCapReportWithPlatform(t *testing.T) {
	platformReceived := false
	var receivedPlatform string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Parse the request body
			var report DataCapReport
			if err := json.NewDecoder(r.Body).Decode(&report); err == nil {
				if report.Platform != "" {
					platformReceived = true
					receivedPlatform = report.Platform
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"throttle":false,"remainingBytes":9663676416,"capLimit":10737418240,"expiryTime":1700179200}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, 5*time.Second)
	ctx := context.Background()

	report := &DataCapReport{
		DeviceID:    "test-device",
		CountryCode: "US",
		Platform:    "ios",
		BytesUsed:   1048576,
	}

	_, err := client.ReportDataCapConsumption(ctx, report)
	if err != nil {
		t.Fatalf("ReportDataCapConsumption failed: %v", err)
	}

	if !platformReceived {
		t.Error("platform field was not received")
	}

	if receivedPlatform != "ios" {
		t.Errorf("expected platform=ios, got %s", receivedPlatform)
	}
}

func TestDataCapClientWithHTTPS(t *testing.T) {
	client := NewClient("datacap-sidecar.example.com", 5*time.Second)

	if client.baseURL != "https://datacap-sidecar.example.com" {
		t.Errorf("expected HTTPS URL, got %s", client.baseURL)
	}
}

func TestDataCapClientEnvironmentVariable(t *testing.T) {
	t.Setenv("DATACAP_URL", "https://env-override.example.com")

	client := NewClient("https://default.example.com", 5*time.Second)

	if client.baseURL != "https://env-override.example.com" {
		t.Errorf("expected environment variable URL, got %s", client.baseURL)
	}
}
