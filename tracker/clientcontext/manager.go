package clientcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

var _ (adapter.ConnectionTracker) = (*Manager)(nil)

type clientInfoKey struct{}

// ContextWithClientInfo returns a new context with the given ClientInfo.
func ContextWithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	return context.WithValue(ctx, clientInfoKey{}, info)
}

// ClientInfoFromContext retrieves the ClientInfo from the context.
func ClientInfoFromContext(ctx context.Context) (ClientInfo, bool) {
	info, ok := ctx.Value(clientInfoKey{}).(ClientInfo)
	return info, ok
}

// Manager is a ConnectionTracker that manages ClientInfo for connections.
type Manager struct {
	logger   log.ContextLogger
	trackers []adapter.ConnectionTracker

	matchBounds  MatchBounds
	inboundRule  *boundsRule
	outboundRule *boundsRule
	ruleMu       sync.RWMutex
}

// NewManager creates a new ClientContext Manager.
func NewManager(bounds MatchBounds, logger log.ContextLogger) *Manager {
	return &Manager{
		trackers:     []adapter.ConnectionTracker{},
		logger:       logger,
		matchBounds:  bounds,
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
	}
}

// AppendTracker appends a ConnectionTracker to the Manager.
func (m *Manager) AppendTracker(tracker adapter.ConnectionTracker) {
	m.trackers = append(m.trackers, tracker)
}

func (m *Manager) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	if !m.match(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	c := &readConn{
		Conn:   conn,
		reader: conn,
		mgr:    m,
	}
	info, err := c.readInfo()
	if err != c.readErr {
		m.logger.Error("failed to read client info ", "tag", "clientcontext-tracker", "error", err)
	}
	if err != nil {
		return c
	}
	if info == nil {
		return c
	}
	ctx = ContextWithClientInfo(ctx, *info)
	conn = c
	for _, tracker := range m.trackers {
		conn = tracker.RoutedConnection(ctx, conn, metadata, matchedRule, matchOutbound)
	}
	return conn
}

func (m *Manager) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	if !m.match(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	c := &readPacketConn{
		PacketConn: conn,
		mgr:        m,
	}
	info, err := c.readInfo()
	if err != c.readErr {
		m.logger.Error("failed to read client info ", "tag", "clientcontext-tracker", "error", err)
	}
	if err != nil {
		return c
	}
	if info == nil {
		return c
	}
	ctx = ContextWithClientInfo(ctx, *info)
	conn = c
	for _, tracker := range m.trackers {
		conn = tracker.RoutedPacketConnection(ctx, conn, metadata, matchedRule, matchOutbound)
	}
	return conn
}

func (m *Manager) match(inbound, outbound string) bool {
	m.ruleMu.RLock()
	defer m.ruleMu.RUnlock()
	return m.inboundRule.match(inbound) && m.outboundRule.match(outbound)
}

func (m *Manager) SetBounds(bounds MatchBounds) {
	m.ruleMu.Lock()
	m.matchBounds = bounds
	m.inboundRule = newBoundsRule(bounds.Inbound)
	m.outboundRule = newBoundsRule(bounds.Outbound)
	m.ruleMu.Unlock()
}

func (m *Manager) MatchBounds() MatchBounds {
	m.ruleMu.RLock()
	defer m.ruleMu.RUnlock()
	return m.matchBounds.clone()
}

// readConn reads client info from the connection on creation.
type readConn struct {
	net.Conn
	mgr     *Manager
	reader  io.Reader
	n       int
	readErr error
}

func (c *readConn) Read(b []byte) (n int, err error) {
	if c.readErr != nil {
		return c.n, c.readErr
	}
	return c.reader.Read(b)
}

// readInfo reads and decodes client info, then sends an HTTP 200 OK response.
func (c *readConn) readInfo() (*ClientInfo, error) {
	var buf [32]byte
	n, err := c.Conn.Read(buf[:])
	if err != nil {
		c.readErr = err
		c.n = n
		return nil, err
	}
	if !bytes.HasPrefix(buf[:n], []byte(packetPrefix)) {
		c.reader = io.MultiReader(bytes.NewReader(buf[:n]), c.Conn)
		return nil, nil
	}

	var info ClientInfo
	reader := io.MultiReader(bytes.NewReader(buf[len(packetPrefix):n]), c.Conn)
	if err := json.NewDecoder(reader).Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding client info: %w", err)
	}

	if _, err := c.Write([]byte("OK")); err != nil {
		return nil, fmt.Errorf("writing OK response: %w", err)
	}
	return &info, nil
}

type readPacketConn struct {
	N.PacketConn
	mgr         *Manager
	destination metadata.Socksaddr
	readErr     error
}

func (c *readPacketConn) ReadPacket(b *buf.Buffer) (destination metadata.Socksaddr, err error) {
	if c.readErr != nil {
		return c.destination, c.readErr
	}
	return c.PacketConn.ReadPacket(b)
}

// readInfo reads and decodes client info if the first packet is a CLIENTINFO packet, then sends an
// OK response.
func (c *readPacketConn) readInfo() (*ClientInfo, error) {
	buffer := buf.NewPacket()
	defer buffer.Release()

	destination, err := c.ReadPacket(buffer)
	if err != nil {
		c.destination = destination
		c.readErr = err
		return nil, err
	}
	data := buffer.Bytes()
	if !bytes.HasPrefix(data, []byte(packetPrefix)) {
		// not a client info packet, wrap with cached packet conn so the packet can be read again
		c.PacketConn = bufio.NewCachedPacketConn(c.PacketConn, buffer, destination)
		return nil, nil
	}
	var info ClientInfo
	if err := json.Unmarshal(data[len(packetPrefix):], &info); err != nil {
		return nil, fmt.Errorf("unmarshaling client info: %w", err)
	}

	// CRITICAL: Use a new buffer for the response to ensure we have enough headroom
	// for the packet headers (e.g. VMess). Reusing the old buffer with Reset()
	// discards the headroom and causes 'buffer overflow' panics.
	respBuffer := buf.NewPacket()
	defer respBuffer.Release()
	respBuffer.WriteString("OK")
	if err := c.WritePacket(respBuffer, destination); err != nil {
		return nil, fmt.Errorf("writing OK response: %w", err)
	}
	return &info, nil
}
