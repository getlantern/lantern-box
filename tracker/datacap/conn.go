package datacap

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// Throttle speed constants for datacap enforcement
const (

	// Default upload speed (not throttled to allow user uploads even when capped)
	defaultUploadSpeedBytesPerSec = 640 * 1024 // 5 Mb/s (640 KB/s)

	lowTierSpeedBytesPerSec = 16 * 1024 // 128 Kb/s (16 KB/s)
)

// Conn wraps a net.Conn and tracks data consumption for datacap reporting.
type Conn struct {
	trafficCategory string
	hasRead         atomic.Bool
	hasWritten      atomic.Bool
	counted         atomic.Bool // bidirectional connection added to usage
	net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	client *Client
	logger log.ContextLogger

	clientInfo clientcontext.ClientInfo

	usage        *deviceUsage
	reporter     *Reporter
	ownsReporter bool
	closed       atomic.Bool

	// Throttling
	throttler *Throttler
}

// ConnConfig holds configuration for creating a datacap-tracked connection.
type ConnConfig struct {
	TrafficCategory string
	Conn            net.Conn
	Client          *Client
	Logger          log.ContextLogger
	ClientInfo      clientcontext.ClientInfo
	// Reporter aggregates usage with the device's other connections. When
	// nil, the connection gets its own reporter, flushing every
	// ReportInterval and once more on Close.
	Reporter       *Reporter
	ReportInterval time.Duration
	Throttler      *Throttler // Shared throttler from registry
}

// NewConn creates a new datacap-tracked connection wrapper.
func NewConn(config ConnConfig) *Conn {
	ctx, cancel := context.WithCancel(context.Background())

	// Default to disabled throttler if not provided
	throttler := config.Throttler
	if throttler == nil {
		throttler = NewThrottler(0) // Disabled by default
	}

	reporter, owns := config.Reporter, false
	if reporter == nil {
		reporter = NewReporter(config.Client, config.Logger, config.ReportInterval)
		owns = true
	}

	return &Conn{
		trafficCategory: config.TrafficCategory,
		Conn:            config.Conn,
		ctx:             ctx,
		cancel:          cancel,
		client:          config.Client,
		logger:          config.Logger,
		clientInfo:      config.ClientInfo,
		usage:           reporter.acquire(config.ClientInfo, throttler),
		reporter:        reporter,
		ownsReporter:    owns,
		throttler:       throttler,
	}
}

// Read tracks bytes received and applies throttling if enabled.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.usage.addBytes(int64(n), c.trafficCategory)
		if c.trafficCategory != "" {
			c.hasRead.Store(true)
			c.countIfBidirectional()
		}

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
		c.usage.addBytes(int64(n), c.trafficCategory)
		if c.trafficCategory != "" {
			c.hasWritten.Store(true)
			c.countIfBidirectional()
		}

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

// countIfBidirectional counts the connection once it has both read and
// written bytes.
func (c *Conn) countIfBidirectional() {
	if c.hasRead.Load() && c.hasWritten.Load() && c.counted.CompareAndSwap(false, true) {
		c.usage.addConnection(c.trafficCategory)
	}
}

// Close releases the connection's usage and closes the underlying connection.
// Its last bytes go out with the reporter's next flush, or right away when the
// connection owns its reporter.
func (c *Conn) Close() error {
	if c.closed.Swap(true) {
		return nil // Already closed
	}

	// Cancel context to release throttle waits
	c.cancel()

	c.reporter.release(c.usage)
	if c.ownsReporter {
		c.reporter.Close()
	}

	return c.Conn.Close()
}

// sendReport flushes the reporter's pending usage now.
func (c *Conn) sendReport() {
	c.reporter.flush(context.Background())
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

// GetBytesConsumed returns the bytes of this connection's device not yet
// accepted by the sidecar.
func (c *Conn) GetBytesConsumed() int64 {
	return c.usage.pendingBytes()
}

func (c *Conn) Upstream() any {
	return c.Conn
}
