// Package clientcontext provides a [adapter.ConnectionTracker] that sends and receives client
// metadata after connection handshake. The metadata is stored in the context for other trackers
// to use.
//
// To use this tracker, create a [ClientContextTracker] with either [NewClientContextTracker], for
// clients, or [NewClientContextReader], for servers, then pass it to router.AppendTracker. The
// metadata can be retrieved from the context using [service.PtrFromContext].
// Note that both client and server sides must use this tracker for it to work.
package clientcontext

import (
	stdbufio "bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

var (
	_ (adapter.ConnectionTracker)    = (*ClientContextTracker)(nil)
	_ (N.ConnHandshakeSuccess)       = (*writeConn)(nil)
	_ (N.PacketConnHandshakeSuccess) = (*writePacketConn)(nil)
)

// ClientInfo holds information about the client user/device.
type ClientInfo struct {
	DeviceID    string
	Platform    string
	IsPro       bool
	CountryCode string
	Version     string
}

// MatchBounds specifies inbound and outbound matching rules.
// The empty string is treated as a wildcard.
type MatchBounds struct {
	Inbound  []string
	Outbound []string
}

// ClientContextTracker tracks client context for connections.
type ClientContextTracker struct {
	info         ClientInfo
	inboundRule  *boundsRule
	outboundRule *boundsRule
	logger       log.ContextLogger
	isReader     bool
}

// NewClientContextTracker creates a tracker for writing client info.
func NewClientContextTracker(info ClientInfo, bounds MatchBounds, logger log.ContextLogger) *ClientContextTracker {
	return &ClientContextTracker{
		info:         info,
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
		logger:       logger,
	}
}

// NewClientContextReader creates a tracker for reading client info.
func NewClientContextReader(bounds MatchBounds, logger log.ContextLogger) *ClientContextTracker {
	return &ClientContextTracker{
		inboundRule:  newBoundsRule(bounds.Inbound),
		outboundRule: newBoundsRule(bounds.Outbound),
		logger:       logger,
		isReader:     true,
	}
}

// RoutedConnection wraps the connection for reading or writing client info.
func (t *ClientContextTracker) RoutedConnection(
	ctx context.Context,
	conn net.Conn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) net.Conn {
	if !t.inboundRule.match(metadata.Inbound) || !t.outboundRule.match(matchOutbound.Tag()) {
		return conn
	}
	if t.isReader {
		return newReadConn(ctx, conn, t.logger)
	}
	return newWriteConn(ctx, conn, &t.info, t.logger)
}

// RoutedPacketConnection wraps the packet connection for reading or writing client info.
func (t *ClientContextTracker) RoutedPacketConnection(
	ctx context.Context,
	conn N.PacketConn,
	metadata adapter.InboundContext,
	matchedRule adapter.Rule,
	matchOutbound adapter.Outbound,
) N.PacketConn {
	if !t.inboundRule.match(metadata.Inbound) || !t.outboundRule.match(matchOutbound.Tag()) {
		return conn
	}
	if t.isReader {
		return newReadPacketConn(ctx, conn, t.logger)
	}
	return newWritePacketConn(ctx, conn, metadata, &t.info, t.logger)
}

func (t *ClientContextTracker) UpdateBounds(bounds MatchBounds) {
	t.inboundRule = newBoundsRule(bounds.Inbound)
	t.outboundRule = newBoundsRule(bounds.Outbound)
}

type boundsRule struct {
	tags     []string
	tagMap   map[string]bool
	matchAny bool
}

func newBoundsRule(tags []string) *boundsRule {
	br := &boundsRule{tags: tags, tagMap: make(map[string]bool)}
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

// since sing-box only wraps inbound connections with trackers, conn on the client is from the user
// (e.g. tun connection), while conn on the server is from an outbound on the client. The connection
// to the server isn't established until after conn is wrapped on the client side and we don't have
// access to it until after the handshake.
//
//                     Client                         Server
//                  -------------                 -------------
//    conn    --->  tracker(conn)                       |
// (i.e. tun)            |                              |
//                   dial server   ----------->       conn
//                       |                              |
//                       +<--------  handshake  ------->+
//                       |                              |
//                handshakeSuccess   <----------   tracker(conn)
//                       |                              |
//                send client info   --------->  read client info
//                       |                             |
//                  pipe traffic                 dial upstream
//                                                    ...
//                                                pipe traffic
//
// This is why writeConn (client) doesn't send the client info until ConnHandshakeSuccess while
// readConn (server) reads it immediately upon creation.

// readConn reads client info from the connection on creation.
type readConn struct {
	net.Conn
	ctx    context.Context
	info   ClientInfo
	logger log.ContextLogger

	reader  io.Reader
	n       int
	readErr error
}

// newReadConn creates a readConn and reads client info from it. If successful, the info is stored
// in the context.
func newReadConn(ctx context.Context, conn net.Conn, logger log.ContextLogger) net.Conn {
	c := &readConn{
		Conn:   conn,
		ctx:    ctx,
		reader: conn,
		logger: logger,
	}
	if err := c.readInfo(); err != nil {
		logger.Warn("reading client info: ", err)
	}
	return c
}

func (c *readConn) Read(b []byte) (n int, err error) {
	if c.readErr != nil {
		return c.n, c.readErr
	}
	return c.reader.Read(b)
}

// readInfo reads and decodes client info, then sends an HTTP 200 OK response.
func (c *readConn) readInfo() error {
	var buf [32]byte
	n, err := c.Conn.Read(buf[:])
	if err != nil {
		c.readErr = err
		c.n = n
		return err
	}
	reader := io.MultiReader(bytes.NewReader(buf[:n]), c.Conn)
	if !bytes.HasPrefix(buf[:n], []byte("POST /clientinfo")) {
		c.reader = reader
		return nil
	}

	var info ClientInfo
	req, err := http.ReadRequest(stdbufio.NewReader(reader))
	if err != nil {
		return fmt.Errorf("reading HTTP request: %w", err)
	}
	defer req.Body.Close()
	if err := json.NewDecoder(req.Body).Decode(&info); err != nil {
		return fmt.Errorf("decoding client info: %w", err)
	}
	c.info = info

	resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	if _, err := c.Write([]byte(resp)); err != nil {
		return fmt.Errorf("writing HTTP response: %w", err)
	}
	service.ContextWithPtr(c.ctx, &info)
	return nil
}

// writeConn sends client info after handshake.
type writeConn struct {
	net.Conn
	ctx    context.Context
	info   *ClientInfo
	logger log.ContextLogger
}

func newWriteConn(ctx context.Context, conn net.Conn, info *ClientInfo, logger log.ContextLogger) net.Conn {
	return &writeConn{Conn: conn, ctx: ctx, info: info, logger: logger}
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

const prefix = "CLIENTINFO "

type readPacketConn struct {
	N.PacketConn
	ctx    context.Context
	info   *ClientInfo
	logger log.ContextLogger

	reader      io.Reader
	destination metadata.Socksaddr
	readErr     error
}

// newReadPacketConn creates a readPacketConn and reads client info from it. If successful, the
// info is stored in the context.
func newReadPacketConn(ctx context.Context, conn N.PacketConn, logger log.ContextLogger) N.PacketConn {
	c := &readPacketConn{
		PacketConn: conn,
		ctx:        ctx,
		logger:     logger,
	}
	if err := c.readInfo(); err != nil {
		logger.Warn("reading client info: ", err)
	}
	return c
}

func (c *readPacketConn) ReadPacket(b *buf.Buffer) (destination metadata.Socksaddr, err error) {
	if c.readErr != nil {
		return c.destination, c.readErr
	}
	return c.PacketConn.ReadPacket(b)
}

// readInfo reads and decodes client info if the first packet is a CLIENTINFO packet, then sends an
// OK response.
func (c *readPacketConn) readInfo() error {
	buffer := buf.NewPacket()
	defer buffer.Release()

	destination, err := c.ReadPacket(buffer)
	if err != nil {
		c.readErr = err
		return err
	}
	data := buffer.Bytes()
	if !bytes.HasPrefix(data, []byte(prefix)) {
		// not a client info packet, wrap with cached packet conn so the packet can be read again
		c.PacketConn = bufio.NewCachedPacketConn(c.PacketConn, buffer, destination)
		return nil
	}
	var info ClientInfo
	if err := json.Unmarshal(data[len(prefix):], &info); err != nil {
		return fmt.Errorf("unmarshaling client info: %w", err)
	}
	c.info = &info

	buffer.Reset()
	buffer.WriteString("OK")
	if err := c.WritePacket(buffer, destination); err != nil {
		return fmt.Errorf("writing OK response: %w", err)
	}
	service.ContextWithPtr(c.ctx, &info)
	return nil
}

type writePacketConn struct {
	N.PacketConn
	ctx      context.Context
	metadata adapter.InboundContext
	info     *ClientInfo
	logger   log.ContextLogger
}

func newWritePacketConn(
	ctx context.Context,
	conn N.PacketConn,
	metadata adapter.InboundContext,
	info *ClientInfo,
	logger log.ContextLogger,
) N.PacketConn {
	return &writePacketConn{
		PacketConn: conn,
		ctx:        ctx,
		metadata:   metadata,
		info:       info,
		logger:     logger,
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
	packet := append([]byte(prefix), buf...)
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
