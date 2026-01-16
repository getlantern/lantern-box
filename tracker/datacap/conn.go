package datacap

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// Throttle speed constants for datacap enforcement
const (

	// Default upload speed (not throttled to allow user uploads even when capped)
	defaultUploadSpeedBytesPerSec = 5 * 1024 * 1024 // 5 MB/s

	lowTierSpeedBytesPerSec = 128 * 1024 // 128 KB/s
)

// Conn wraps a net.Conn and tracks data consumption for datacap reporting.
type Conn struct {
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	client *Client
	logger log.ContextLogger

	clientInfo clientcontext.ClientInfo

	// Atomic counters for thread-safe tracking
	bytesSent     atomic.Int64
	bytesReceived atomic.Int64

	// Reporting control
	reportTicker *time.Ticker
	reportMutex  sync.Mutex
	closed       atomic.Bool
	wg           sync.WaitGroup

	// Throttling
	throttler *Throttler
}

// ConnConfig holds configuration for creating a datacap-tracked connection.
type ConnConfig struct {
	Conn           net.Conn
	Client         *Client
	Logger         log.ContextLogger
	ClientInfo     clientcontext.ClientInfo
	ReportInterval time.Duration
	Throttler      *Throttler // Shared throttler from registry
}

// NewConn creates a new datacap-tracked connection wrapper.
func NewConn(config ConnConfig) *Conn {
	ctx, cancel := context.WithCancel(context.Background())

	// Default report interval to 10 seconds if not specified
	if config.ReportInterval == 0 {
		config.ReportInterval = 10 * time.Second
	}

	// Default to disabled throttler if not provided
	throttler := config.Throttler
	if throttler == nil {
		throttler = NewThrottler(0) // Disabled by default
	}

	conn := &Conn{
		Conn:         config.Conn,
		ctx:          ctx,
		cancel:       cancel,
		client:       config.Client,
		logger:       config.Logger,
		clientInfo:   config.ClientInfo,
		reportTicker: time.NewTicker(config.ReportInterval),
		throttler:    throttler,
	}

	// Start periodic reporting goroutine
	conn.wg.Add(1)
	go conn.periodicReport()

	return conn
}

// Read tracks bytes received and applies throttling if enabled.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.bytesReceived.Add(int64(n))

		// Apply throttling after read (token bucket wait)
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitRead(c.ctx, n); waitErr != nil {
				// Context cancelled, but we already read the data
				// Return the data and the error
				return n, waitErr
			}
		}
	}
	return
}

// Write tracks bytes sent and applies throttling if enabled.
func (c *Conn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		c.bytesSent.Add(int64(n))

		// Apply throttling after write (token bucket wait)
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitWrite(c.ctx, n); waitErr != nil {
				// Context cancelled, but we already wrote the data
				// Return the bytes written and the error
				return n, waitErr
			}
		}
	}
	return
}

// Close stops reporting and closes the underlying connection.
func (c *Conn) Close() error {
	if c.closed.Swap(true) {
		return nil // Already closed
	}

	// Stop the reporting ticker
	c.reportTicker.Stop()

	// Cancel context to signal goroutines to stop
	c.cancel()

	// Wait for all goroutines to finish
	c.wg.Wait()

	// Send final report
	c.sendReport()

	return c.Conn.Close()
}

// periodicReport runs in a goroutine and periodically reports data consumption.
func (c *Conn) periodicReport() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.reportTicker.C:
			c.sendReport()
		}
	}
}

// sendReport sends the delta consumption data to the sidecar.
func (c *Conn) sendReport() {
	c.reportMutex.Lock()
	defer c.reportMutex.Unlock()

	if c.client == nil {
		return
	}
	sent := c.bytesSent.Swap(0)
	received := c.bytesReceived.Swap(0)
	delta := sent + received

	if delta == 0 {
		return
	}

	report := &DataCapReport{
		DeviceID:    c.clientInfo.DeviceID,
		CountryCode: c.clientInfo.CountryCode,
		Platform:    c.clientInfo.Platform,
		BytesUsed:   delta,
	}

	timeout := c.client.httpClient.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	reportCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	status, err := c.client.ReportDataCapConsumption(reportCtx, report)
	if err != nil {
		// On failure, add bytes back for next report attempt
		c.bytesSent.Add(sent)
		c.bytesReceived.Add(received)
		c.logger.Debug("failed to report datacap consumption (will retry): ", err)
	} else {
		c.logger.Debug("reported datacap delta: ", delta, " bytes (sent: ", sent, ", received: ", received, ") for device ", c.clientInfo.DeviceID)
		if status != nil {
			c.updateThrottleState(status)
		}
	}
}

// updateThrottleState updates the throttling configuration based on the current status.
func (c *Conn) updateThrottleState(status *DataCapStatus) {
	if c.throttler == nil {
		c.logger.Debug("throttler not initialized, skipping update")
		return
	}

	if status.Throttle {
		// We want to throttle Download (Write) but keep Upload (Read) fast enough for requests.
		c.throttler.UpdateRates(defaultUploadSpeedBytesPerSec, lowTierSpeedBytesPerSec)
		c.logger.Debug("data cap exhausted, throttling at ", lowTierSpeedBytesPerSec, " bytes/s")
	} else {
		c.throttler.Disable()
		c.logger.Debug("throttling disabled - full speed")
	}
}

// GetStatus queries the sidecar for current data cap status.
func (c *Conn) GetStatus() (*DataCapStatus, error) {
	// Skip if client is nil (datacap disabled)
	if c.client == nil {
		return nil, nil
	}

	// Use the client's configured timeout for consistency
	timeout := c.client.httpClient.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second // Fallback if client has no timeout set
	}
	statusCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return c.client.GetDataCapStatus(statusCtx, c.clientInfo.DeviceID)
}

// GetBytesConsumed returns the total bytes consumed by this connection.
func (c *Conn) GetBytesConsumed() int64 {
	return c.bytesSent.Load() + c.bytesReceived.Load()
}
