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
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/ntp"

	lAdapter "github.com/getlantern/lantern-box/adapter"
)

// ErrUnusableInput wraps a failure caused by Run's own arguments: an unusable
// probeURL, or a non-positive timeout. It means the arguments cannot produce a
// probe, not that the outbound or the target failed.
var ErrUnusableInput = errors.New("unusable probe input")

// Run dials out, completes an HTTP GET to probeURL over that one connection,
// discarding the body, and reports how long it took. A request that completes
// is a success whatever status it returned; when probeURL's handler confirms
// receipt, success also means the handler was reached. The connection is closed
// before Run returns.
//
// The reported duration runs from the moment the request has been written to
// the conn to the end of the response body: one round trip through the
// outbound to the target. Connection setup (the dial, any proxy or TLS
// handshake, and a handshake the outbound defers to the first write) is
// excluded, so outbounds that establish connections differently are timed
// alike.
//
// out's DialContext must return once ctx is cancelled or its deadline passes;
// Run returns no sooner than that dial does.
func Run(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration) (time.Duration, error) {
	if probeURL == "" {
		return 0, fmt.Errorf("%w: empty probe URL", ErrUnusableInput)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%w: non-positive timeout %s", ErrUnusableInput, timeout)
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrUnusableInput, err)
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
		return 0, fmt.Errorf("%w: no host or port in probe URL %q", ErrUnusableInput, probeURL)
	}

	probeCtx, cancel := context.WithTimeout(lAdapter.ContextWithProbe(ctx), timeout)
	defer cancel()

	conn, err := out.DialContext(probeCtx, "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
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
	if tp := linkURL.Query().Get("tp"); tp != "" {
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

	start, err := timed.lastWrite(probeCtx)
	if err != nil {
		return 0, fmt.Errorf("await request write: %w", err)
	}
	return time.Since(start), nil
}

// writeTimedConn records when its most recent Write returned, counting only
// writes that started before the first response byte. For a bodyless GET the
// last of those is the request itself, completed only once any handshake the
// outbound runs on first write is done. A write in flight when the response
// arrives still counts; later writes, such as a TLS close_notify at body EOF,
// do not.
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
	n, err := c.Conn.Write(b)
	if counts {
		c.mu.Lock()
		c.last = time.Now()
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
	return c.last, nil
}
