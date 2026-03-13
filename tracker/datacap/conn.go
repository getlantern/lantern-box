package datacap

import (
	"net"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// Conn wraps a net.Conn and tracks data consumption for datacap reporting.
type Conn struct {
	net.Conn
	reportingCore
}

// ConnConfig holds configuration for creating a datacap-tracked connection.
type ConnConfig struct {
	Conn           net.Conn
	Sink           ReportSink
	Logger         log.ContextLogger
	ClientInfo     clientcontext.ClientInfo
	ReportInterval time.Duration
	Throttler      *Throttler // Shared throttler from registry
}

// NewConn creates a new datacap-tracked connection wrapper.
func NewConn(config ConnConfig) *Conn {
	conn := &Conn{
		Conn: config.Conn,
		reportingCore: newReportingCore(reportingConfig{
			Sink:           config.Sink,
			Logger:         config.Logger,
			ClientInfo:     config.ClientInfo,
			ReportInterval: config.ReportInterval,
			Throttler:      config.Throttler,
		}),
	}
	conn.startPeriodicReport()
	return conn
}

// Read tracks bytes received and applies throttling if enabled.
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.bytesReceived.Add(int64(n))
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitRead(c.ctx, n); waitErr != nil {
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
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitWrite(c.ctx, n); waitErr != nil {
				return n, waitErr
			}
		}
	}
	return
}

// Close stops reporting and closes the underlying connection.
func (c *Conn) Close() error {
	if !c.closeReporting() {
		return nil
	}
	return c.Conn.Close()
}

func (c *Conn) GetStatus() (*DataCapStatus, error) { return c.getStatus() }
func (c *Conn) GetBytesConsumed() int64             { return c.getBytesConsumed() }
func (c *Conn) Upstream() any                       { return c.Conn }
