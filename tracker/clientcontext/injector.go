package clientcontext

import (
	stdbufio "bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	N "github.com/sagernet/sing/common/network"
)

var (
	_ (adapter.ConnectionTracker)    = (*ClientContextInjector)(nil)
	_ (N.ConnHandshakeSuccess)       = (*writeConn)(nil)
	_ (N.PacketConnHandshakeSuccess) = (*writePacketConn)(nil)
)

// ClientContextInjector is a connection tracker that sends client info to a ClientContextManager.
type ClientContextInjector struct {
	inboundRule  *boundsRule
	outboundRule *boundsRule
	logger       log.ContextLogger
	info         ClientInfo
}

// NewClientContextInjector creates a tracker for injecting client info.
func NewClientContextInjector(info ClientInfo, bounds MatchBounds, logger log.ContextLogger) *ClientContextInjector {
	return &ClientContextInjector{
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
		logger:       logger,
		info:         info,
	}
}

// RoutedConnection wraps the connection for writing client info.
func (t *ClientContextInjector) RoutedConnection(
	ctx context.Context,
	conn net.Conn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) net.Conn {
	if !t.inboundRule.match(metadata.Inbound) || !t.outboundRule.match(matchOutbound.Tag()) {
		return conn
	}
	return newWriteConn(conn, &t.info)
}

// RoutedPacketConnection wraps the packet connection for writing client info.
func (t *ClientContextInjector) RoutedPacketConnection(
	ctx context.Context,
	conn N.PacketConn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) N.PacketConn {
	if !t.inboundRule.match(metadata.Inbound) || !t.outboundRule.match(matchOutbound.Tag()) {
		return conn
	}
	return newWritePacketConn(conn, metadata, &t.info)
}

func (t *ClientContextInjector) UpdateBounds(bounds MatchBounds) {
	t.inboundRule = newBoundsRule(bounds.Inbound)
	t.outboundRule = newBoundsRule(bounds.Outbound)
}

// writeConn sends client info after handshake.
type writeConn struct {
	net.Conn
	info *ClientInfo
}

func newWriteConn(conn net.Conn, info *ClientInfo) net.Conn {
	return &writeConn{Conn: conn, info: info}
}

// ConnHandshakeSuccess sends client info upon successful handshake with the server.
func (c *writeConn) ConnHandshakeSuccess(conn net.Conn) error {
	if err := c.sendInfo(conn); err != nil {
		return fmt.Errorf("sending client info: %w", err)
	}
	return nil
}

// sendInfo marshals and sends client info as an HTTP POST, then waits for HTTP 200 OK.
func (c *writeConn) sendInfo(conn net.Conn) error {
	buf, err := json.Marshal(c.info)
	if err != nil {
		return fmt.Errorf("marshaling client info: %w", err)
	}
	// Write HTTP POST request
	req := bytes.NewBuffer(nil)
	fmt.Fprintf(req, "POST /clientinfo HTTP/1.1\r\n")
	fmt.Fprintf(req, "Host: localhost\r\n")
	fmt.Fprintf(req, "Content-Type: application/json\r\n")
	fmt.Fprintf(req, "Content-Length: %d\r\n", len(buf))
	fmt.Fprintf(req, "\r\n")
	req.Write(buf)
	if _, err = conn.Write(req.Bytes()); err != nil {
		return fmt.Errorf("writing client info: %w", err)
	}

	// wait for HTTP 200 OK response
	reader := stdbufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return fmt.Errorf("reading HTTP response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("invalid server response: %s", resp.Status)
	}
	return nil
}

type writePacketConn struct {
	N.PacketConn
	metadata adapter.InboundContext
	info     *ClientInfo
}

func newWritePacketConn(
	conn N.PacketConn,
	metadata adapter.InboundContext,
	info *ClientInfo,
) N.PacketConn {
	return &writePacketConn{
		PacketConn: conn,
		metadata:   metadata,
		info:       info,
	}
}

// PacketConnHandshakeSuccess sends client info upon successful handshake.
func (c *writePacketConn) PacketConnHandshakeSuccess(conn net.PacketConn) error {
	if err := c.sendInfo(conn); err != nil {
		return fmt.Errorf("sending client info: %w", err)
	}
	return nil
}

// sendInfo marshals and sends client info as a CLIENTINFO packet, then waits for OK.
func (c *writePacketConn) sendInfo(conn net.PacketConn) error {
	buf, err := json.Marshal(c.info)
	if err != nil {
		return fmt.Errorf("marshaling client info: %w", err)
	}
	packet := append([]byte(packetPrefix), buf...)
	_, err = conn.WriteTo(packet, c.metadata.Destination)
	if err != nil {
		return fmt.Errorf("writing packet: %w", err)
	}

	// wait for `OK` response
	resp := make([]byte, 2)
	if _, _, err := conn.ReadFrom(resp); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if string(resp) != "OK" {
		return fmt.Errorf("invalid response: %s", resp)
	}
	return nil
}
