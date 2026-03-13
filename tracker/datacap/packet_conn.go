package datacap

import (
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
	reportingCore
}

// PacketConnConfig holds configuration for creating a datacap-tracked packet connection.
type PacketConnConfig struct {
	Conn           N.PacketConn
	Sink           ReportSink
	Logger         log.ContextLogger
	ClientInfo     clientcontext.ClientInfo
	ReportInterval time.Duration
	Throttler      *Throttler // Shared throttler from registry
}

// NewPacketConn creates a new datacap-tracked packet connection wrapper.
func NewPacketConn(config PacketConnConfig) *PacketConn {
	conn := &PacketConn{
		PacketConn: config.Conn,
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

// ReadPacket tracks bytes received from packet reading and applies throttling.
func (c *PacketConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		return dest, err
	}
	if buffer.Len() > 0 {
		c.bytesReceived.Add(int64(buffer.Len()))
		if c.throttler.IsEnabled() {
			if waitErr := c.throttler.WaitRead(c.ctx, buffer.Len()); waitErr != nil {
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
	if !c.closeReporting() {
		return nil
	}
	return c.PacketConn.Close()
}

func (c *PacketConn) GetStatus() (*DataCapStatus, error) { return c.getStatus() }
func (c *PacketConn) GetBytesConsumed() int64             { return c.getBytesConsumed() }
func (c *PacketConn) Upstream() any                       { return c.PacketConn }
