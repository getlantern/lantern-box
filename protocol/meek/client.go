// Package meek implements a domain-fronted meek client: chunked
// TCP-over-HTTPS, session-keyed by a per-Conn random ID sent in
// X-Session-Id. The wire format is the meek-v1 polling scheme as used
// by Psiphon and Lantern.
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
	"strconv"
	"sync"
	"time"
)

const (
	defaultPollIntervalMs = 100
	// defaultMaxBodyBytes is the per-poll body cap. Throughput is bytes-per-poll
	// ÷ round-trip-time; each poll is a full HTTPS request through the front, so
	// the RTT (not the poll interval) paces them. 256 KiB keeps a healthy stream
	// moving without making a single lost/retried poll expensive to replay.
	defaultMaxBodyBytes = 256 * 1024
	// legacyMaxBodyBytes is the response cap the server applies to clients that
	// don't advertise X-Meek-Max-Body — i.e. pre-negotiation clients that read
	// at most 64 KiB. Keeps old clients from truncating a larger response.
	legacyMaxBodyBytes      = 64 * 1024
	defaultSessionIDLen     = 16
	defaultReadTimeout      = 30 * time.Second
	defaultMaxWriteBufBytes = 1 << 20 // 1 MiB
	// defaultMaxPollRetries is how many times a failed poll is retried before the
	// session is torn down. Retries are safe because each poll carries a
	// monotonic X-Meek-Seq the server dedupes (it replays the buffered response
	// for a repeated seq), so a lost request or lost response can't dup or drop
	// bytes — see roundtrip / the server's seq handling.
	defaultMaxPollRetries = 4
	retryBaseBackoff      = 250 * time.Millisecond
	headerSeq             = "X-Meek-Seq"
	headerMaxBody         = "X-Meek-Max-Body"
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
	// MaxPollRetries is how many times a failed poll is retried (with backoff)
	// before the session is torn down. <=0 uses defaultMaxPollRetries. Retries
	// are made safe by the per-poll X-Meek-Seq the server dedupes.
	MaxPollRetries int
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
	if c.MaxPollRetries <= 0 {
		c.MaxPollRetries = defaultMaxPollRetries
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

	// seq is the monotonic sequence number of the next poll; the server dedupes
	// on it so a retried poll replays rather than re-applies. inflight is the
	// chunk taken for the current seq, held until the poll succeeds so a retry
	// resends the same bytes; inflightTaken distinguishes "not yet taken" from
	// "taken, empty" (an empty poll still has a seq).
	seq           uint64
	inflight      []byte
	inflightTaken bool

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
	total := len(p)
	// Append in chunks bounded by the remaining backlog capacity so a
	// single large Write can't grow writeBuf past MaxWriteBufBytes: each
	// pass blocks until the poll loop has drained room, applying real
	// backpressure rather than only checking the cap before a wholesale
	// append.
	for len(p) > 0 {
		c.mu.Lock()
		for c.writeBuf.Len() >= c.cfg.MaxWriteBufBytes && !c.closed {
			if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
				c.mu.Unlock()
				return total - len(p), errWriteDeadline
			}
			c.writeCond.Wait()
		}
		if c.closed {
			c.mu.Unlock()
			return total - len(p), errors.New("meek: closed")
		}
		if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
			c.mu.Unlock()
			return total - len(p), errWriteDeadline
		}
		room := c.cfg.MaxWriteBufBytes - c.writeBuf.Len()
		n := len(p)
		if n > room {
			n = room
		}
		c.writeBuf.Write(p[:n])
		p = p[n:]
		c.mu.Unlock()

		select {
		case c.writeReady <- struct{}{}:
		default:
		}
	}
	return total, nil
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

		if err := c.roundtripWithRetry(); err != nil {
			if errors.Is(err, io.EOF) {
				c.markClosed(nil) // clean end-of-stream → Read returns io.EOF
			} else {
				c.markClosed(err)
			}
			return
		}
	}
}

// roundtripWithRetry retries a failed poll up to MaxPollRetries with linear
// backoff. Safe because the poll keeps the same seq + in-flight chunk across
// attempts: the server replays the buffered response for a repeated seq (no dup
// upstream, no dropped downstream). Aborts promptly if the conn is cancelled.
func (c *Conn) roundtripWithRetry() error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * retryBaseBackoff)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return c.ctx.Err()
			case <-timer.C:
			}
		}
		if err := c.roundtrip(); err != nil {
			lastErr = err
			var perm *permanentError
			if errors.As(err, &perm) {
				return err // session is gone — retrying would resurrect it
			}
			if attempt >= c.cfg.MaxPollRetries {
				return fmt.Errorf("poll failed after %d retries: %w", c.cfg.MaxPollRetries, lastErr)
			}
			continue
		}
		return nil
	}
}

func (c *Conn) roundtrip() error {
	c.mu.Lock()
	if !c.inflightTaken {
		c.inflight = c.takeWriteChunkLocked() // may be nil for an empty (poll-only) request
		c.inflightTaken = true
	}
	bodyBytes := c.inflight
	seq := c.seq
	c.mu.Unlock()

	// Bound each poll by ReadTimeout so one hung request can't block the poll loop
	// forever when the caller's HTTPClient has no timeout of its own. Cancel runs
	// after the response body is fully read below (deferred LIFO, after Body.Close).
	reqCtx, cancel := context.WithTimeout(c.ctx, c.cfg.ReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.URL, bytes.NewReader(bodyBytes))
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
	req.Header.Set(headerSeq, strconv.FormatUint(seq, 10))
	// Advertise how large a response we can read, so the server can send bigger
	// chunks without truncating older clients (which omit this header).
	req.Header.Set(headerMaxBody, strconv.Itoa(c.cfg.MaxBodyBytes))

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Non-200 means the server dropped the session (it does so on every error
		// path), so a retry would resurrect a fresh session instead of ending the
		// stream — mark it permanent. A clean upstream-closed 410 maps to io.EOF so
		// Read surfaces a proper end-of-stream.
		if resp.StatusCode == http.StatusGone {
			return &permanentError{io.EOF}
		}
		return &permanentError{fmt.Errorf("meek: status %d", resp.StatusCode)}
	}

	// Read one byte past the negotiated cap so an over-cap response is detected and
	// rejected, not silently truncated (which would corrupt the tunneled byte stream).
	// Mirrors the server's request-size check.
	limited := io.LimitReader(resp.Body, int64(c.cfg.MaxBodyBytes)+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(buf) > c.cfg.MaxBodyBytes {
		return fmt.Errorf("meek: response body exceeds negotiated max %d bytes", c.cfg.MaxBodyBytes)
	}
	// The poll succeeded: commit the response, release the in-flight chunk, and
	// advance the seq so the next poll is a fresh one.
	c.mu.Lock()
	if len(buf) > 0 {
		c.readBuf.Write(buf)
		c.readCond.Broadcast()
	}
	c.inflight = nil
	c.inflightTaken = false
	c.seq++
	c.mu.Unlock()
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
	case "Host", "Content-Type", "X-Session-Id", headerSeq, headerMaxBody:
		return true
	default:
		return false
	}
}

type meekAddr string

func (a meekAddr) Network() string { return "meek" }
func (a meekAddr) String() string  { return string(a) }

// permanentError marks a poll failure that retrying won't fix — specifically a
// non-200 response, since the server drops the session on every error path, so a
// retry just resurrects a fresh session instead of surfacing end-of-stream.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// timeoutError implements net.Error with Timeout()==true so the meek Conn behaves
// like a real net.Conn for callers that switch on net.Error/Timeout (the stdlib
// http client, io helpers, etc.) to distinguish a deadline from a hard failure.
type timeoutError struct{ msg string }

func (e *timeoutError) Error() string   { return e.msg }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

var (
	errReadDeadline  net.Error = &timeoutError{"meek: read deadline exceeded"}
	errWriteDeadline net.Error = &timeoutError{"meek: write deadline exceeded"}
)
