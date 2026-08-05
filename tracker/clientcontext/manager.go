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
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

var _ (adapter.ConnectionTracker) = (*Manager)(nil)

// InfoCarrier carries ClientInfo decoded from a client-context frame.
type InfoCarrier interface {
	ClientInfo() (ClientInfo, bool)
}

// InfoFromConn returns ClientInfo from a Manager-wrapped connection. It reports
// false if none is present.
func InfoFromConn(conn any) (ClientInfo, bool) {
	carrier, ok := common.Cast[InfoCarrier](conn)
	if !ok {
		return ClientInfo{}, false
	}
	return carrier.ClientInfo()
}

// Manager decodes ClientInfo from client-context frames and exposes it on the
// wrapped connection.
type Manager struct {
	logger log.ContextLogger

	matchBounds  MatchBounds
	inboundRule  *boundsRule
	outboundRule *boundsRule
	ruleMu       sync.RWMutex
}

// NewManager returns a new Manager.
func NewManager(bounds MatchBounds, logger log.ContextLogger) *Manager {
	return &Manager{
		logger:       logger,
		matchBounds:  bounds,
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
	}
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
	c.info = info
	return c
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
	c.info = info
	return c
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
	info    *ClientInfo
	n       int
	readErr error
}

func (c *readConn) Read(b []byte) (n int, err error) {
	if c.readErr != nil {
		return c.n, c.readErr
	}
	return c.reader.Read(b)
}

func (c *readConn) ClientInfo() (ClientInfo, bool) {
	if c.info == nil {
		return ClientInfo{}, false
	}
	return *c.info, true
}

// matchMarker reports the frame marker at the start of b and whether it expects
// ackResponse.
func matchMarker(b []byte) (marker string, ack bool) {
	switch {
	case bytes.HasPrefix(b, []byte(packetPrefix)):
		return packetPrefix, false
	case bytes.HasPrefix(b, []byte(legacyPacketPrefix)):
		return legacyPacketPrefix, true
	default:
		return "", false
	}
}

// readInfo reads and decodes a client-info frame. If the stream does not begin
// with one, it restores the consumed bytes and returns (nil, nil).
func (c *readConn) readInfo() (*ClientInfo, error) {
	var buf [32]byte
	// Read enough bytes to distinguish both markers even if the stream splits
	// them across reads.
	n, err := io.ReadAtLeast(c.Conn, buf[:], len(packetPrefix))
	if n == 0 {
		// Preserve the original read error so callers can distinguish connection
		// failure from decode failure.
		c.readErr = err
		c.n = n
		return nil, err
	}
	marker, ack := matchMarker(buf[:n])
	// Treat short reads and non-markers as ordinary traffic.
	if err != nil || marker == "" {
		c.reader = io.MultiReader(bytes.NewReader(buf[:n]), c.Conn)
		return nil, nil
	}

	var info ClientInfo
	reader := io.MultiReader(bytes.NewReader(buf[len(marker):n]), c.Conn)
	dec := json.NewDecoder(reader)
	if err := dec.Decode(&info); err != nil {
		return nil, fmt.Errorf("decoding client info: %w", err)
	}
	// Restore any bytes buffered past the JSON frame.
	leftover, _ := io.ReadAll(dec.Buffered()) // reads the decoder's own buffer: cannot fail
	if len(leftover) > 0 {
		c.reader = io.MultiReader(bytes.NewReader(leftover), c.Conn)
	}
	if ack {
		if _, err := c.Write([]byte(ackResponse)); err != nil {
			return nil, fmt.Errorf("writing %s response: %w", ackResponse, err)
		}
	}
	return &info, nil
}

func (c *readConn) Upstream() any {
	return c.Conn
}

type readPacketConn struct {
	N.PacketConn
	mgr         *Manager
	info        *ClientInfo
	destination metadata.Socksaddr
	readErr     error
}

func (c *readPacketConn) ReadPacket(b *buf.Buffer) (destination metadata.Socksaddr, err error) {
	if c.readErr != nil {
		return c.destination, c.readErr
	}
	return c.PacketConn.ReadPacket(b)
}

func (c *readPacketConn) ClientInfo() (ClientInfo, bool) {
	if c.info == nil {
		return ClientInfo{}, false
	}
	return *c.info, true
}

// readInfo reads and decodes client info from the first packet when present.
// Otherwise it caches the packet for replay.
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
	marker, ack := matchMarker(data)
	if marker == "" {
		// Cache the packet so it can be replayed as ordinary traffic.
		c.PacketConn = bufio.NewCachedPacketConn(c.PacketConn, buffer, destination)
		return nil, nil
	}
	var info ClientInfo
	if err := json.Unmarshal(data[len(marker):], &info); err != nil {
		return nil, fmt.Errorf("unmarshaling client info: %w", err)
	}
	if ack {
		if err := c.writeAck(destination); err != nil {
			return nil, err
		}
	}
	return &info, nil
}

// writeAck replies to a legacy client with ackResponse using a fresh packet
// buffer with header headroom.
func (c *readPacketConn) writeAck(destination metadata.Socksaddr) error {
	respBuffer := buf.NewPacket()
	defer respBuffer.Release()
	respBuffer.Advance(N.CalculateFrontHeadroom(c))
	respBuffer.Reserve(N.CalculateRearHeadroom(c))
	respBuffer.WriteString(ackResponse)
	if err := c.WritePacket(respBuffer, destination); err != nil {
		return fmt.Errorf("writing %s response: %w", ackResponse, err)
	}
	return nil
}

func (c *readPacketConn) Upstream() any {
	return c.PacketConn
}
