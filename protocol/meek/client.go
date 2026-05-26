// Package meek implements a domain-fronted meek client following the
// Tor pluggable-transport meek v1 wire format: chunked TCP-over-HTTPS,
// session-keyed by a per-Conn random ID sent in X-Session-Id.
//
// Each Conn maintains a single polling goroutine that POSTs to the meek
// server every PollIntervalMs, batching outbound bytes from Write into
// the request body and feeding the response body to readers via Read.
// The server is expected to be a meek endpoint behind any front the
// client dials — typically a Lantern-operated /meek/ endpoint reachable
// through Akamai or CloudFront via the inner Host.
package meek

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	defaultPollIntervalMs   = 100
	defaultMaxBodyBytes     = 64 * 1024
	defaultSessionIDLen     = 16
	defaultReadTimeout      = 30 * time.Second
	defaultMaxWriteBufBytes = 1 << 20 // 1 MiB
)

// Config is the runtime configuration for a meek Conn.
type Config struct {
	URL          string
	InnerHost    string
	ExtraHeaders map[string]string
	HTTPClient   *http.Client
	PollInterval time.Duration
	MaxBodyBytes int
	SessionIDLen int
	ReadTimeout  time.Duration
	// MaxWriteBufBytes caps the unsent Write backlog. Write blocks once
	// the buffer reaches this size and resumes as the poll loop drains
	// it, so a sender outpacing a slow front applies backpressure
	// instead of growing memory without bound.
	MaxWriteBufBytes int
}

func (c *Config) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Duration(defaultPollIntervalMs) * time.Millisecond
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.SessionIDLen <= 0 {
		c.SessionIDLen = defaultSessionIDLen
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = defaultReadTimeout
	}
	if c.MaxWriteBufBytes <= 0 {
		c.MaxWriteBufBytes = defaultMaxWriteBufBytes
	}
}

// Conn is a net.Conn that tunnels through a meek server.
type Conn struct {
	cfg       Config
	sessionID string

	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	writeBuf   bytes.Buffer
	writeReady chan struct{}
	// writeCond wakes a Write blocked on a full writeBuf when the poll
	// loop drains it (or on close / write-deadline).
	writeCond *sync.Cond

	readBuf  bytes.Buffer
	readCond *sync.Cond

	closed   bool
	closeErr error

	readDeadline       time.Time
	readDeadlineTimer  *time.Timer
	writeDeadline      time.Time
	writeDeadlineTimer *time.Timer

	pollDone chan struct{}
}

// Dial opens a meek session. The supplied HTTP client must be configured
// so its TLS DialContext targets a working front — typically the radiance
// fronted/scanner package's output composed with the standard fronted
// dialer.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	cfg.applyDefaults()

	if cfg.URL == "" {
		return nil, errors.New("meek: empty URL")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("meek: parse URL: %w", err)
	}
	if cfg.InnerHost == "" {
		cfg.InnerHost = u.Host
	}
	if cfg.HTTPClient == nil {
		return nil, errors.New("meek: HTTPClient required")
	}

	id := make([]byte, cfg.SessionIDLen)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("meek: session id: %w", err)
	}

	c := &Conn{
		cfg:        cfg,
		sessionID:  hex.EncodeToString(id),
		writeReady: make(chan struct{}, 1),
		pollDone:   make(chan struct{}),
	}
	c.readCond = sync.NewCond(&c.mu)
	c.writeCond = sync.NewCond(&c.mu)
	c.ctx, c.cancel = context.WithCancel(ctx)

	go c.pollLoop()
	return c, nil
}

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for c.readBuf.Len() == 0 && !c.closed {
		if !c.readDeadline.IsZero() && time.Now().After(c.readDeadline) {
			return 0, errReadDeadline
		}
		c.readCond.Wait()
	}
	if c.readBuf.Len() == 0 && c.closed {
		if c.closeErr != nil {
			return 0, c.closeErr
		}
		return 0, io.EOF
	}
	return c.readBuf.Read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	c.mu.Lock()
	// Block while the backlog is at the cap so a fast sender applies
	// backpressure instead of growing writeBuf without bound.
	for c.writeBuf.Len() >= c.cfg.MaxWriteBufBytes && !c.closed {
		if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
			c.mu.Unlock()
			return 0, errWriteDeadline
		}
		c.writeCond.Wait()
	}
	if c.closed {
		c.mu.Unlock()
		return 0, errors.New("meek: closed")
	}
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		c.mu.Unlock()
		return 0, errWriteDeadline
	}
	c.writeBuf.Write(p)
	c.mu.Unlock()

	select {
	case c.writeReady <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.readDeadlineTimer != nil {
		c.readDeadlineTimer.Stop()
		c.readDeadlineTimer = nil
	}
	if c.writeDeadlineTimer != nil {
		c.writeDeadlineTimer.Stop()
		c.writeDeadlineTimer = nil
	}
	c.readCond.Broadcast()
	c.writeCond.Broadcast()
	c.mu.Unlock()

	c.cancel()
	<-c.pollDone
	return nil
}

func (c *Conn) LocalAddr() net.Addr  { return meekAddr("meek-client") }
func (c *Conn) RemoteAddr() net.Addr { return meekAddr(c.cfg.URL) }

func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline arranges for a parked Read to wake when t elapses.
// readCond.Wait has no native timeout, so without an active signal a
// Read would park past the deadline until data, close, or a new
// deadline arrived. A zero t clears the deadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	if c.readDeadlineTimer != nil {
		c.readDeadlineTimer.Stop()
		c.readDeadlineTimer = nil
	}
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.readCond.Broadcast()
		return nil
	}
	c.readDeadlineTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		c.readCond.Broadcast()
		c.mu.Unlock()
	})
	return nil
}

// SetWriteDeadline arranges for a Write blocked on a full backlog to
// wake when t elapses (mirrors SetReadDeadline): without an active
// signal the writeCond.Wait would park past the deadline if the front
// is hung and the poll loop never drains. A zero t clears the deadline.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	if c.writeDeadlineTimer != nil {
		c.writeDeadlineTimer.Stop()
		c.writeDeadlineTimer = nil
	}
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		c.writeCond.Broadcast()
		return nil
	}
	c.writeDeadlineTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		c.writeCond.Broadcast()
		c.mu.Unlock()
	})
	return nil
}

func (c *Conn) pollLoop() {
	defer close(c.pollDone)
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.markClosed(c.ctx.Err())
			return
		case <-ticker.C:
		case <-c.writeReady:
		}

		if err := c.roundtrip(); err != nil {
			c.markClosed(err)
			return
		}
	}
}

func (c *Conn) roundtrip() error {
	c.mu.Lock()
	bodyBytes := c.takeWriteChunkLocked()
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Host = c.cfg.InnerHost
	// Apply caller headers first, then set the protocol-critical ones so
	// config can't override the session keying or framing (e.g. pinning a
	// fixed X-Session-Id across conns, or changing Content-Type). Host is
	// set via req.Host above and likewise not overridable here.
	for k, v := range c.cfg.ExtraHeaders {
		if isReservedHeader(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Session-Id", c.sessionID)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("meek: status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(c.cfg.MaxBodyBytes))
	buf, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(buf) > 0 {
		c.mu.Lock()
		c.readBuf.Write(buf)
		c.readCond.Broadcast()
		c.mu.Unlock()
	}
	return nil
}

func (c *Conn) takeWriteChunkLocked() []byte {
	if c.writeBuf.Len() == 0 {
		return nil
	}
	chunk := c.writeBuf.Bytes()
	if len(chunk) > c.cfg.MaxBodyBytes {
		chunk = chunk[:c.cfg.MaxBodyBytes]
	}
	out := make([]byte, len(chunk))
	copy(out, chunk)
	c.writeBuf.Next(len(chunk))
	// Wake any Write parked on the backlog cap.
	c.writeCond.Broadcast()
	return out
}

func (c *Conn) markClosed(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.closeErr = err
	}
	c.readCond.Broadcast()
	c.writeCond.Broadcast()
	c.mu.Unlock()
}

// isReservedHeader reports whether name is a header the meek protocol
// owns and config must not override.
func isReservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Content-Type", "X-Session-Id":
		return true
	default:
		return false
	}
}

type meekAddr string

func (a meekAddr) Network() string { return "meek" }
func (a meekAddr) String() string  { return string(a) }

var (
	errReadDeadline  = errors.New("meek: read deadline exceeded")
	errWriteDeadline = errors.New("meek: write deadline exceeded")
)
