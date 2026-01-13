package datacap

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// PacketConn wraps a sing-box network.PacketConn and tracks data consumption for datacap reporting.
type PacketConn struct {
	N.PacketConn
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

// PacketConnConfig holds configuration for creating a datacap-tracked packet connection.
type PacketConnConfig struct {
	Conn           N.PacketConn
	Client         *Client
	Logger         log.ContextLogger
	ClientInfo     clientcontext.ClientInfo
	ReportInterval time.Duration
	ThrottleSpeed  int64
}

// NewPacketConn creates a new datacap-tracked packet connection wrapper.
func NewPacketConn(config PacketConnConfig) *PacketConn {
	ctx, cancel := context.WithCancel(context.Background())

	// Default report interval to 10 seconds if not specified
	if config.ReportInterval == 0 {
		config.ReportInterval = 10 * time.Second
	}

	conn := &PacketConn{
		PacketConn:   config.Conn,
		ctx:          ctx,
		cancel:       cancel,
		client:       config.Client,
		logger:       config.Logger,
		clientInfo:   config.ClientInfo,
		reportTicker: time.NewTicker(config.ReportInterval),
		throttler:    NewThrottler(config.ThrottleSpeed),
	}

	// Start periodic reporting goroutine
	conn.wg.Add(1)
	go conn.periodicReport()

	return conn
}

// ReadPacket tracks bytes received from packet reading and applies throttling.
func (c *PacketConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}

	if buffer.Len() > 0 {
		c.bytesReceived.Add(int64(buffer.Len()))

		// Apply throttling after read (token bucket wait)
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitRead(c.ctx, buffer.Len()); waitErr != nil {
				// Context cancelled, but we already read the data
				return dest, waitErr
			}
		}
	}
	return dest, nil
}

// WritePacket tracks bytes sent from packet writing and applies throttling.
func (c *PacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	packetSize := buffer.Len()

	if packetSize > 0 {
		c.bytesSent.Add(int64(packetSize))

		// Apply throttling before write (token bucket wait)
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitWrite(c.ctx, packetSize); waitErr != nil {
				return waitErr
			}
		}
	}

	return c.PacketConn.WritePacket(buffer, destination)
}

// Close stops reporting and closes the underlying packet connection.
func (c *PacketConn) Close() error {
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

	return c.PacketConn.Close()
}

// periodicReport runs in a goroutine and periodically reports data consumption.
func (c *PacketConn) periodicReport() {
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
func (c *PacketConn) sendReport() {
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
func (c *PacketConn) updateThrottleState(status *DataCapStatus) {
	if c.throttler == nil {
		c.logger.Debug("throttler not initialized, skipping update")
		return
	}

	if status.Throttle {
		c.throttler.EnableWithRates(lowTierSpeedBytesPerSec, defaultUploadSpeedBytesPerSec)
		c.logger.Debug("data cap exhausted, throttling at ", lowTierSpeedBytesPerSec, " bytes/s")
	} else {
		c.throttler.Disable()
		c.logger.Debug("throttling disabled - full speed")
	}
}

// GetStatus queries the sidecar for current data cap status.
func (c *PacketConn) GetStatus() (*DataCapStatus, error) {
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

// GetBytesConsumed returns the total bytes consumed by this packet connection.
func (c *PacketConn) GetBytesConsumed() int64 {
	return c.bytesSent.Load() + c.bytesReceived.Load()
}
