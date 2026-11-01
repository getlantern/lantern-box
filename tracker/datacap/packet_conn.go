package datacap

import (
	"context"
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
	// A UDP association can carry several destinations; do not apply one
	// connection-level hostname to all packets. Its bytes count as unknown,
	// and no UDP connection count is inferred.
	trafficCategory string
	N.PacketConn
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

// PacketConnConfig holds configuration for creating a datacap-tracked packet connection.
type PacketConnConfig struct {
	TrafficCategories bool
	Conn              N.PacketConn
	Client            *Client
	Logger            log.ContextLogger
	ClientInfo        clientcontext.ClientInfo
	// Reporter aggregates usage with the device's other connections. When
	// nil, the connection gets its own reporter, flushing every
	// ReportInterval and once more on Close.
	Reporter       *Reporter
	ReportInterval time.Duration
	Throttler      *Throttler // Shared throttler from registry
}

// NewPacketConn creates a new datacap-tracked packet connection wrapper.
func NewPacketConn(config PacketConnConfig) *PacketConn {
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

	category := ""
	if config.TrafficCategories {
		category = "unknown"
	}
	return &PacketConn{
		trafficCategory: category,
		PacketConn:      config.Conn,
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

// ReadPacket tracks bytes received from packet reading and applies throttling.
func (c *PacketConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}

	if buffer.Len() > 0 {
		c.usage.addBytes(int64(buffer.Len()), c.trafficCategory)

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
		c.usage.addBytes(int64(packetSize), c.trafficCategory)

		// Apply throttling before write (token bucket wait)
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitWrite(c.ctx, packetSize); waitErr != nil {
				return waitErr
			}
		}
	}

	return c.PacketConn.WritePacket(buffer, destination)
}

// Close releases the connection's usage and closes the underlying packet
// connection. Its last bytes go out with the reporter's next flush, or right
// away when the connection owns its reporter.
func (c *PacketConn) Close() error {
	if c.closed.Swap(true) {
		return nil // Already closed
	}

	// Cancel context to release throttle waits
	c.cancel()

	c.reporter.release(c.usage)
	if c.ownsReporter {
		c.reporter.Close()
	}

	return c.PacketConn.Close()
}

// sendReport flushes the reporter's pending usage now.
func (c *PacketConn) sendReport() {
	c.reporter.flush(context.Background())
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

// GetBytesConsumed returns the bytes of this connection's device not yet
// accepted by the sidecar.
func (c *PacketConn) GetBytesConsumed() int64 {
	return c.usage.pendingBytes()
}

func (c *PacketConn) Upstream() any {
	return c.PacketConn
}
