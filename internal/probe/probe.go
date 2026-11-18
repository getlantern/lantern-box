// Package probe dials an outbound and completes one HTTP request through it,
// timed.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/ntp"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

// Some servers block requests without a browser User-Agent.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.8037.97 Safari/537.36"

// ErrUnusableInput wraps a failure caused by Run's or Measure's own arguments:
// an unusable probeURL, or a non-positive timeout. It means the arguments
// cannot produce a probe, not that the outbound or the target failed.
var ErrUnusableInput = errors.New("unusable probe input")

// Each of these identifies the stage at which a Measure call failed.
var (
	ErrDial = errors.New("dial")
	// ErrRequest means no response headers arrived; the path may never have
	// opened, for an outbound that defers its handshake to first use.
	ErrRequest = errors.New("do request")
	// ErrResponseBody means the response arrived and its body then failed
	// partway, so the target was reached.
	ErrResponseBody = errors.New("read response body")
)

// Result reports what one Measure observed. Its durations span the handshake and
// the request, plus the dial, unless the dial succeeded for an outbound that
// defers its handshake to first use.
type Result struct {
	HTTPStatus    int
	TimeToHeaders time.Duration
	// Elapsed spans up to the failure, or through reading the body.
	Elapsed time.Duration
	// BytesRead counts body bytes, stopping at the limit Measure was given.
	BytesRead int64
	// ThroughputBytesPerSecond is BytesRead over the time spent reading them.
	ThroughputBytesPerSecond float64
}

// Run dials out, completes an HTTP GET to probeURL over that one connection,
// discarding the body, and reports how long it took. A request that completes
// is a success whatever status it returned; when probeURL's handler confirms
// receipt, success also means the handler was reached. The connection is closed
// before Run returns.
//
// The reported duration runs from the moment the request has been written to
// the conn to the end of the response body. The dial, any proxy or TLS
// handshake, and a handshake the outbound defers to the first write are
// excluded. For an outbound that sends the request before its server has
// connected to the target, that connect falls inside the timed window.
//
// out's DialContext must return once ctx is cancelled or its deadline passes;
// Run returns no sooner than that dial does.
func Run(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration) (time.Duration, error) {
	destination, err := probeDestination(probeURL)
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%w: non-positive timeout %s", ErrUnusableInput, timeout)
	}

	probeCtx, cancel := context.WithTimeout(lAdapter.ContextWithProbe(ctx), timeout)
	defer cancel()

	conn, err := out.DialContext(probeCtx, "tcp", destination)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	timed := newWriteTimedConn(conn)
	traceCtx := httptrace.WithClientTrace(probeCtx, &httptrace.ClientTrace{
		GotFirstResponseByte: timed.freeze,
	})
	req, err := http.NewRequestWithContext(traceCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	if tp := req.URL.Query().Get("tp"); tp != "" {
		req.Header.Set("traceparent", tp)
	}

	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return timed, nil
			},
			TLSClientConfig: &tls.Config{
				Time:    ntp.TimeFuncFromContext(probeCtx),
				RootCAs: A.RootPoolFromContext(probeCtx),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("do request: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	end := time.Now()

	start, err := timed.lastWrite(probeCtx)
	if err != nil {
		return 0, fmt.Errorf("await request write: %w", err)
	}
	return end.Sub(start), nil
}

// Measure dials out, completes an HTTP GET to probeURL over that one connection,
// and records what it observed. It reads at most maxBytes of the response body;
// a non-positive maxBytes reads all of it. Unlike Run, a body that fails partway
// is an error, ErrResponseBody. Any error other than an ErrUnusableInput one
// wraps ErrDial, ErrRequest, or ErrResponseBody, and the result holds whatever
// had been observed by then. The connection is closed before Measure returns.
//
// out's DialContext must return once ctx is cancelled or its deadline passes;
// Measure returns no sooner than that dial does.
func Measure(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration, maxBytes int64) (Result, error) {
	var result Result
	destination, err := probeDestination(probeURL)
	if err != nil {
		return result, err
	}
	if timeout <= 0 {
		return result, fmt.Errorf("%w: non-positive timeout %s", ErrUnusableInput, timeout)
	}

	probeCtx, cancel := context.WithTimeout(lAdapter.ContextWithProbe(ctx), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrUnusableInput, err)
	}
	req.Header.Set("User-Agent", userAgent)
	if tp := req.URL.Query().Get("tp"); tp != "" {
		req.Header.Set("traceparent", tp)
	}
	// Built before the clock starts, so its setup is not charged to the
	// measurement: the transport closure reads conn, which is dialed below and
	// set before Do runs.
	var conn net.Conn
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return conn, nil
			},
			TLSClientConfig: &tls.Config{
				Time:    ntp.TimeFuncFromContext(probeCtx),
				RootCAs: A.RootPoolFromContext(probeCtx),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	start := time.Now()
	conn, err = out.DialContext(probeCtx, "tcp", destination)
	if err != nil {
		result.Elapsed = time.Since(start)
		return result, fmt.Errorf("%w: %w", ErrDial, err)
	}
	defer conn.Close()
	if earlyConn, ok := common.Cast[N.EarlyConn](conn); ok && earlyConn.NeedHandshake() {
		start = time.Now()
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Elapsed = time.Since(start)
		return result, fmt.Errorf("%w: %w", ErrRequest, err)
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	result.TimeToHeaders = time.Since(start)

	body := io.Reader(resp.Body)
	if maxBytes > 0 {
		body = io.LimitReader(resp.Body, maxBytes)
	}
	bodyStart := time.Now()
	result.BytesRead, err = io.Copy(io.Discard, body)
	bodyElapsed := time.Since(bodyStart)
	result.Elapsed = time.Since(start)
	if bodyElapsed > 0 {
		result.ThroughputBytesPerSecond = float64(result.BytesRead) / bodyElapsed.Seconds()
	}
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrResponseBody, err)
	}
	return result, nil
}

// ValidateURL returns the ErrUnusableInput error Run would return for probeURL
// before dialing, or nil if Run would dial.
func ValidateURL(probeURL string) error {
	_, err := probeDestination(probeURL)
	return err
}

func probeDestination(probeURL string) (M.Socksaddr, error) {
	if probeURL == "" {
		return M.Socksaddr{}, fmt.Errorf("%w: empty probe URL", ErrUnusableInput)
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return M.Socksaddr{}, fmt.Errorf("%w: %w", ErrUnusableInput, err)
	}
	hostname := linkURL.Hostname()
	port := linkURL.Port()
	if port == "" {
		switch linkURL.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	if hostname == "" || port == "" {
		return M.Socksaddr{}, fmt.Errorf("%w: no host or port in probe URL %q", ErrUnusableInput, probeURL)
	}
	return M.ParseSocksaddrHostPortStr(hostname, port), nil
}

// writeTimedConn records when its most recent Write returned, counting only
// writes that started before the first response byte. For a bodyless GET the
// last of those is the request itself, completed only once any handshake the
// outbound runs on first write is done. Later writes, such as a TLS
// close_notify at body EOF, do not count. A write still in flight when the
// response arrives counts from when it started, since its return says nothing
// about when its bytes left.
type writeTimedConn struct {
	net.Conn
	mu       sync.Mutex
	last     time.Time
	frozen   bool
	inFlight int
	settled  chan struct{}
}

func newWriteTimedConn(conn net.Conn) *writeTimedConn {
	return &writeTimedConn{Conn: conn, settled: make(chan struct{})}
}

func (c *writeTimedConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	counts := !c.frozen
	if counts {
		c.inFlight++
	}
	c.mu.Unlock()
	called := time.Now()
	n, err := c.Conn.Write(b)
	if counts {
		c.mu.Lock()
		if c.frozen {
			c.last = called
		} else {
			c.last = time.Now()
		}
		c.inFlight--
		c.settleLocked()
		c.mu.Unlock()
	}
	return n, err
}

func (c *writeTimedConn) freeze() {
	c.mu.Lock()
	if !c.frozen {
		c.frozen = true
		c.settleLocked()
	}
	c.mu.Unlock()
}

func (c *writeTimedConn) settleLocked() {
	if c.frozen && c.inFlight == 0 {
		close(c.settled)
	}
}

// errNoRequestWrite is returned when the response began before any write.
var errNoRequestWrite = errors.New("response began before the request was written")

// lastWrite waits for every counted write to return, then reports when the
// last of them did.
func (c *writeTimedConn) lastWrite(ctx context.Context) (time.Time, error) {
	select {
	case <-c.settled:
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.IsZero() {
		return time.Time{}, errNoRequestWrite
	}
	return c.last, nil
}
