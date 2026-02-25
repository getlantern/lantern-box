package datacap

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client handles communication with the datacap sidecar service.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// ClientConfig holds configuration for the datacap client.
type ClientConfig struct {
	BaseURL            string
	Timeout            time.Duration
	InsecureSkipVerify bool
}

// NewClient creates a new datacap client.
// The baseURL can be overridden by the DATACAP_URL environment variable.
// Supports both HTTP and HTTPS. For HTTPS, uses system's trusted certificates by default.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return NewClientWithConfig(ClientConfig{
		BaseURL:            baseURL,
		Timeout:            timeout,
		InsecureSkipVerify: false,
	})
}

// NewClientWithConfig creates a new datacap client with advanced configuration.
func NewClientWithConfig(config ClientConfig) *Client {
	// Check for environment variable override
	if envURL := os.Getenv("DATACAP_URL"); envURL != "" {
		config.BaseURL = envURL
	}

	// Ensure HTTPS if not explicitly HTTP
	if config.BaseURL != "" && !strings.HasPrefix(config.BaseURL, "http://") && !strings.HasPrefix(config.BaseURL, "https://") {
		config.BaseURL = "https://" + config.BaseURL
	}

	// Create HTTP client with TLS configuration
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: config.InsecureSkipVerify,
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   config.Timeout,
			Transport: transport,
		},
		baseURL: config.BaseURL,
	}
}

// DataCapStatus represents the response from the GET /data-cap/{deviceId} endpoint.
type DataCapStatus struct {
	Throttle   bool  `json:"throttle"`
	CapLimit   int64 `json:"capLimit"`
	ExpiryTime int64 `json:"expiryTime"`
}

// DataCapReport represents the request body for POST /data-cap/ endpoint.
type DataCapReport struct {
	DeviceID    string `json:"deviceId"`
	CountryCode string `json:"countryCode"`
	Platform    string `json:"platform"`
	BytesUsed   int64  `json:"bytesUsed"`
}

func (c *Client) GetDataCapStatus(ctx context.Context, deviceID string) (*DataCapStatus, error) {
	url := fmt.Sprintf("%s/data-cap/%s", c.baseURL, deviceID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query datacap status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("datacap status request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var status DataCapStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode datacap status: %w", err)
	}

	return &status, nil
}

// ReportDataCapConsumption sends data consumption report to the sidecar.
// Endpoint: POST /data-cap/
// This tracks usage and returns updated cap status.
func (c *Client) ReportDataCapConsumption(ctx context.Context, report *DataCapReport) (*DataCapStatus, error) {
	url := fmt.Sprintf("%s/data-cap/", c.baseURL)

	jsonData, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal report: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to report datacap consumption: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("datacap report request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var status DataCapStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode datacap status: %w", err)
	}

	return &status, nil
}
