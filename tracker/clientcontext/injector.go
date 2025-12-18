package clientcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
)

var (
	_ (adapter.ConnectionTracker)    = (*ClientContextInjector)(nil)
	_ (N.ConnHandshakeSuccess)       = (*writeConn)(nil)
	_ (N.PacketConnHandshakeSuccess) = (*writePacketConn)(nil)
)

// ClientContextInjector is a connection tracker that sends client info to a ClientContext Manager.
type ClientContextInjector struct {
	getInfo      GetClientInfoFn
	inboundRule  *boundsRule
	outboundRule *boundsRule
	ruleMu       sync.RWMutex
}

// NewClientContextInjector creates a tracker for injecting client info.
func NewClientContextInjector(fn GetClientInfoFn, bounds MatchBounds) *ClientContextInjector {
	return &ClientContextInjector{
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
		getInfo:      fn,
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
	if !t.match(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	info := t.getInfo()
	return newWriteConn(conn, &info)
}

// RoutedPacketConnection wraps the packet connection for writing client info.
func (t *ClientContextInjector) RoutedPacketConnection(
	ctx context.Context,
	conn N.PacketConn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) N.PacketConn {
	if !t.match(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	info := t.getInfo()
	return newWritePacketConn(conn, metadata, &info)
}

func (t *ClientContextInjector) match(inbound, outbound string) bool {
	t.ruleMu.RLock()
	defer t.ruleMu.RUnlock()
	return t.inboundRule.match(inbound) && t.outboundRule.match(outbound)
}

func (t *ClientContextInjector) UpdateBounds(bounds MatchBounds) {
	t.ruleMu.Lock()
	t.inboundRule = newBoundsRule(bounds.Inbound)
	t.outboundRule = newBoundsRule(bounds.Outbound)
	t.ruleMu.Unlock()
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
	packet := append([]byte(packetPrefix), buf...)
	if _, err = conn.Write(packet); err != nil {
		return fmt.Errorf("writing client info: %w", err)
	}

	// wait for `OK` response
	var resp [2]byte
	if _, err := conn.Read(resp[:]); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if string(resp[:]) != "OK" {
		return fmt.Errorf("invalid response: %s", resp)
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
	if _, err = conn.WriteTo(packet, c.metadata.Destination); err != nil {
		return fmt.Errorf("writing packet: %w", err)
	}

	// wait for `OK` response
	var resp [2]byte
	if _, _, err := conn.ReadFrom(resp[:]); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if string(resp[:]) != "OK" {
		return fmt.Errorf("invalid response: %s", resp)
	}
	return nil
}
