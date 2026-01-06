package clientcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	lAdapter "github.com/getlantern/lantern-box/adapter"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/protocol/group"
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
	matchBounds  MatchBounds
	inboundRule  *boundsRule
	outboundRule *boundsRule
	ruleMu       sync.RWMutex
}

// NewClientContextInjector creates a tracker for injecting client info.
func NewClientContextInjector(fn GetClientInfoFn, bounds MatchBounds) *ClientContextInjector {
	return &ClientContextInjector{
		matchBounds:  bounds,
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
	_, isGroup := matchOutbound.(adapter.OutboundGroup)
	if !isGroup && !t.preMatch(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	info := t.getInfo()
	t.ruleMu.RLock()
	rule := *t.outboundRule
	t.ruleMu.RUnlock()
	return newWriteConn(conn, &info, rule, matchOutbound)
}

// RoutedPacketConnection wraps the packet connection for writing client info.
func (t *ClientContextInjector) RoutedPacketConnection(
	ctx context.Context,
	conn N.PacketConn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) N.PacketConn {
	_, isGroup := matchOutbound.(adapter.OutboundGroup)
	if !isGroup && !t.preMatch(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	info := t.getInfo()
	t.ruleMu.RLock()
	rule := *t.outboundRule
	t.ruleMu.RUnlock()
	return newWritePacketConn(conn, metadata, &info, rule, matchOutbound)
}

func (t *ClientContextInjector) preMatch(inbound, outbound string) bool {
	t.ruleMu.RLock()
	defer t.ruleMu.RUnlock()
	return t.inboundRule.match(inbound) && t.outboundRule.match(outbound)
}

func (t *ClientContextInjector) SetBounds(bounds MatchBounds) {
	t.ruleMu.Lock()
	t.inboundRule = newBoundsRule(bounds.Inbound)
	t.outboundRule = newBoundsRule(bounds.Outbound)
	t.matchBounds = bounds
	t.ruleMu.Unlock()
}

func (t *ClientContextInjector) MatchBounds() MatchBounds {
	t.ruleMu.RLock()
	defer t.ruleMu.RUnlock()
	return t.matchBounds
}

// writeConn sends client info after handshake.
type writeConn struct {
	net.Conn
	info          *ClientInfo
	outboundRule  boundsRule
	matchOutbound adapter.Outbound
}

func newWriteConn(
	conn net.Conn,
	info *ClientInfo,
	outboundRule boundsRule,
	matchOutbound adapter.Outbound,
) net.Conn {
	return &writeConn{
		Conn:          conn,
		info:          info,
		outboundRule:  outboundRule,
		matchOutbound: matchOutbound,
	}
}

// ConnHandshakeSuccess sends client info upon successful handshake with the server.
func (c *writeConn) ConnHandshakeSuccess(conn net.Conn) error {
	if !c.match(conn) {
		return nil
	}
	if err := c.sendInfo(conn); err != nil {
		return fmt.Errorf("sending client info: %w", err)
	}
	return nil
}

func (c *writeConn) match(conn net.Conn) bool {
	outbound, isGroup := c.matchOutbound.(adapter.OutboundGroup)
	if !isGroup {
		return true // caught by pre-match
	}
	// we need to check if the outbound used by the group matches as it may contain outbounds
	// that don't

	if tconn, ok := conn.(*lAdapter.TaggedConn); ok {
		return c.outboundRule.match(tconn.Tag())
	}
	return c.outboundRule.match(outbound.Now())
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
	metadata      adapter.InboundContext
	info          *ClientInfo
	outboundRule  boundsRule
	matchOutbound adapter.Outbound
}

func newWritePacketConn(
	conn N.PacketConn,
	metadata adapter.InboundContext,
	info *ClientInfo,
	outboundRule boundsRule,
	matchOutbound adapter.Outbound,
) N.PacketConn {
	return &writePacketConn{
		PacketConn:    conn,
		metadata:      metadata,
		info:          info,
		outboundRule:  outboundRule,
		matchOutbound: matchOutbound,
	}
}

// PacketConnHandshakeSuccess sends client info upon successful handshake.
func (c *writePacketConn) PacketConnHandshakeSuccess(conn net.PacketConn) error {
	if !c.match(conn) {
		return nil
	}
	if err := c.sendInfo(conn); err != nil {
		return fmt.Errorf("sending client info: %w", err)
	}
	return nil
}

func (c *writePacketConn) match(conn net.PacketConn) bool {
	outbound, isGroup := c.matchOutbound.(adapter.OutboundGroup)
	if !isGroup {
		return true // caught by pre-match
	}
	// we need to check if the outbound used by the group matches as it may contain outbounds
	// that don't

	// fast path: conn is already tagged
	if tconn, ok := conn.(*lAdapter.TaggedPacketConn); ok {
		return c.outboundRule.match(tconn.Tag())
	}

	switch outbound.(type) {
	case *group.Selector:
		return c.outboundRule.match(outbound.Now())
	case *group.URLTest:
		// edge case: we cannot determine which outbound was actually used. urltest.Now will return
		// the tag for the selected TCP outbound if it's not nil and the selected UDP outbound can
		// be different.
	}
	return false
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
