package clientcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// MatchBounds specifies inbound and outbound matching rules.
// The empty string and "any" are treated as a wildcard.
type MatchBounds struct {
	Inbound  []string
	Outbound []string
}

func (mb MatchBounds) clone() MatchBounds {
	return MatchBounds{
		Inbound:  slices.Clone(mb.Inbound),
		Outbound: slices.Clone(mb.Outbound),
	}
}

type boundsRule struct {
	tagMap   map[string]bool
	matchAny bool
}

func newBoundsRule(tags []string) *boundsRule {
	br := &boundsRule{tagMap: make(map[string]bool)}
	if len(tags) == 1 && (tags[0] == "" || tags[0] == "any") {
		br.matchAny = true
		return br
	}
	for _, tag := range tags {
		br.tagMap[tag] = true
	}
	return br
}

func (b *boundsRule) match(tag string) bool {
	return (b.matchAny && tag != "") || b.tagMap[tag]
}

var _ (adapter.ConnectionTracker) = (*Manager)(nil)

// readInfoTimeout bounds each window of the marker-classification read. The
// Manager runs on every routed connection, so a client that sends nothing until
// the server greets it must not stall the connection here.
var readInfoTimeout = 5 * time.Second

// infoCarrier carries ClientInfo decoded from a client-info frame.
type infoCarrier interface {
	ClientInfo() (ClientInfo, bool)
}

// InfoFromConn returns ClientInfo from a Manager-wrapped connection. It reports
// false if none is present.
func InfoFromConn(conn any) (ClientInfo, bool) {
	carrier, ok := common.Cast[infoCarrier](conn)
	if !ok {
		return ClientInfo{}, false
	}
	return carrier.ClientInfo()
}

// Manager decodes ClientInfo from client-info frames and exposes it on the
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
	c := &readConn{Conn: conn, reader: conn}
	info, err := c.readInfo()
	if err != nil {
		m.logger.Error("failed to read client info ", "tag", "clientcontext-tracker", "error", err)
	}
	c.info = info
	return c
}

func (m *Manager) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	if !m.match(metadata.Inbound, matchOutbound.Tag()) {
		return conn
	}
	c := &readPacketConn{PacketConn: conn}
	info, err := c.readInfo()
	if err != nil {
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

// matchMarker reports the frame marker at the start of b and whether its sender
// expects ackResponse.
func matchMarker(b []byte) (marker string, ack bool) {
	switch {
	case bytes.HasPrefix(b, []byte(clientInfoPrefix)):
		return clientInfoPrefix, false
	case bytes.HasPrefix(b, []byte(legacyClientInfoPrefix)):
		return legacyClientInfoPrefix, true
	default:
		return "", false
	}
}

// readConn reads client info from the connection on creation. The zero info
// means none.
type readConn struct {
	net.Conn
	reader  io.Reader
	info    ClientInfo
	readErr error
	// hasDeadline is set while a caller has a read deadline set, which sends
	// reads back through sing-box's deadline reader.
	hasDeadline atomic.Bool
}

// SetReadDeadline applies t to reads that start after it. A read already
// blocked past sing-box's deadline reader is not interrupted by t.
func (c *readConn) SetReadDeadline(t time.Time) error {
	c.hasDeadline.Store(!t.IsZero())
	return c.Conn.SetReadDeadline(t)
}

// SetDeadline applies t as SetReadDeadline does for reads.
func (c *readConn) SetDeadline(t time.Time) error {
	c.hasDeadline.Store(!t.IsZero())
	return c.Conn.SetDeadline(t)
}

func (c *readConn) Read(b []byte) (n int, err error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.reader.Read(b)
}

func (c *readConn) ClientInfo() (ClientInfo, bool) {
	return c.info, c.info != ClientInfo{}
}

// readInfo reads and decodes a client-info frame, restoring the consumed bytes
// if the stream does not begin with one. A connection that fails before
// sending anything fails its later reads and is not reported as an error.
func (c *readConn) readInfo() (ClientInfo, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(readInfoTimeout))
	defer c.Conn.SetReadDeadline(time.Time{})
	head, err := readHead(c.Conn, nil)
	if errors.Is(err, os.ErrDeadlineExceeded) && len(head) > 0 {
		// A partial marker means the client is mid-frame, not waiting for the
		// server. One more window lets the frame be attributed, and acks a legacy
		// sender before any destination bytes reach it.
		_ = c.Conn.SetReadDeadline(time.Now().Add(readInfoTimeout))
		head, err = readHead(c.Conn, head)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// Classification finishes on the first Read instead, so a frame delayed
		// past the deadline is still kept from the destination, though too late
		// to attribute the connection.
		c.reader = &pendingHead{conn: c, head: head}
		return ClientInfo{}, nil
	}
	if len(head) == 0 {
		c.readErr = err
		return ClientInfo{}, nil
	}
	// Decoding a frame gets its own window, so a client that stalls mid-frame
	// cannot block routing.
	_ = c.Conn.SetReadDeadline(time.Now().Add(readInfoTimeout))
	return c.consumeHead(head)
}

// consumeHead classifies head, the first bytes of the stream: it decodes and
// strips a frame, or restores head as ordinary traffic.
func (c *readConn) consumeHead(head []byte) (ClientInfo, error) {
	marker, ack := matchMarker(head)
	stream := &streamReader{c: c}
	if marker == "" {
		c.reader = io.MultiReader(bytes.NewReader(head), stream)
		return ClientInfo{}, nil
	}

	var info ClientInfo
	reader := io.MultiReader(bytes.NewReader(head[len(marker):]), stream)
	dec := json.NewDecoder(reader)
	if err := dec.Decode(&info); err != nil {
		// The marker and part of the frame are already consumed; fail the
		// connection rather than forward a truncated stream.
		c.readErr = err
		return ClientInfo{}, fmt.Errorf("decoding client info: %w", err)
	}
	// Continue the stream from the decoder: dec.Buffered() holds what it read past
	// the frame, and reader holds what it has not pulled yet.
	c.reader = io.MultiReader(dec.Buffered(), reader)
	if ack {
		if _, err := c.Write([]byte(ackResponse)); err != nil {
			// The legacy sender waits for the ack before sending its traffic.
			c.readErr = err
			return ClientInfo{}, fmt.Errorf("writing %s response: %w", ackResponse, err)
		}
	}
	return info, nil
}

// replaceableReader is a reader that copy loops may bypass, reading
// UpstreamReader instead, while ReaderReplaceable reports true. sing's deadline
// reader is one: it is replaceable when no deadline is set and no read is in
// flight.
type replaceableReader interface {
	ReaderReplaceable() bool
	UpstreamReader() any
}

// streamReader reads the stream after its head. Once sing-box's deadline
// reader is replaceable, reads bypass it unless a caller sets a read deadline.
// That reader runs each read on a new goroutine with a buffer the size of the
// read, held until data arrives, so on a mostly idle proxy it costs a goroutine
// and a buffer per connection.
type streamReader struct {
	c        *readConn
	upstream io.Reader
}

func (s *streamReader) Read(b []byte) (int, error) {
	if s.c.hasDeadline.Load() {
		// A read that times out leaves the deadline reader's read in flight, so
		// reads resume past it only once it is replaceable again.
		s.upstream = nil
		return s.c.Conn.Read(b)
	}
	if s.upstream == nil {
		if r, ok := s.c.Conn.(replaceableReader); ok && r.ReaderReplaceable() {
			s.upstream, _ = r.UpstreamReader().(io.Reader)
		}
		if s.upstream == nil {
			return s.c.Conn.Read(b)
		}
	}
	return s.upstream.Read(b)
}

// readHead reads from r after head until the bytes read either start with a
// marker or can no longer start one. Deciding as soon as possible keeps a
// short opening that is not a frame from waiting for more bytes.
func readHead(r io.Reader, head []byte) ([]byte, error) {
	var buf [32]byte
	n := copy(buf[:], head)
	for undecided(buf[:n]) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return buf[:n], err
		}
	}
	return buf[:n], nil
}

// undecided reports whether head is a proper prefix of a frame marker.
func undecided(head []byte) bool {
	return isProperPrefix(head, clientInfoPrefix) || isProperPrefix(head, legacyClientInfoPrefix)
}

func isProperPrefix(head []byte, marker string) bool {
	return len(head) < len(marker) && strings.HasPrefix(marker, string(head))
}

// pendingHead finishes classifying a stream whose first bytes did not arrive
// within readInfoTimeout. The decoded info is discarded.
type pendingHead struct {
	conn *readConn
	head []byte
}

func (p *pendingHead) Read(b []byte) (int, error) {
	head, err := readHead(p.conn.Conn, p.head)
	p.head = head
	// A timed-out read leaves the stream undecided, so keep the bytes read so far
	// rather than replay a possible marker as ordinary traffic.
	if len(head) == 0 || errors.Is(err, os.ErrDeadlineExceeded) && undecided(head) {
		return 0, err
	}
	if _, err := p.conn.consumeHead(head); err != nil {
		return 0, err
	}
	return p.conn.reader.Read(b)
}

func (c *readConn) Upstream() any {
	return c.Conn
}

// readPacketConn reads client info from the first packet on creation. The zero
// info means none.
type readPacketConn struct {
	N.PacketConn
	info        ClientInfo
	destination metadata.Socksaddr
	readErr     error
}

// ReadPacket drops every datagram that starts with a frame marker. On a
// datagram transport a frame can arrive after the flow's first datagram, and
// forwarding it would leak client info to the destination.
func (c *readPacketConn) ReadPacket(b *buf.Buffer) (destination metadata.Socksaddr, err error) {
	if c.readErr != nil {
		return c.destination, c.readErr
	}
	start := b.Start()
	for {
		destination, err = c.PacketConn.ReadPacket(b)
		if err != nil {
			return destination, err
		}
		if marker, _ := matchMarker(b.Bytes()); marker == "" {
			return destination, nil
		}
		// Keep the caller's front headroom for the next read.
		b.Resize(start, 0)
	}
}

func (c *readPacketConn) ClientInfo() (ClientInfo, bool) {
	return c.info, c.info != ClientInfo{}
}

// readInfo reads and decodes client info from the first packet when present,
// otherwise caching the packet for replay. A failed read fails later reads and
// is not reported as an error.
func (c *readPacketConn) readInfo() (ClientInfo, error) {
	buffer := buf.NewPacket()
	defer buffer.Release()

	destination, err := c.PacketConn.ReadPacket(buffer)
	if err != nil {
		c.destination = destination
		c.readErr = err
		return ClientInfo{}, nil
	}
	data := buffer.Bytes()
	marker, ack := matchMarker(data)
	if marker == "" {
		c.PacketConn = bufio.NewCachedPacketConn(c.PacketConn, buffer, destination)
		return ClientInfo{}, nil
	}
	var info ClientInfo
	if err := json.Unmarshal(data[len(marker):], &info); err != nil {
		return ClientInfo{}, fmt.Errorf("unmarshaling client info: %w", err)
	}
	if ack {
		if err := c.writeAck(destination); err != nil {
			return ClientInfo{}, err
		}
	}
	return info, nil
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
