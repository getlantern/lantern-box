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
	"net/url"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/ntp"
)

// ErrUnusableInput wraps a failure caused by Run's own arguments: an unusable
// probeURL, or a non-positive timeout. It means the arguments cannot produce a
// probe, not that the outbound or the target failed.
var ErrUnusableInput = errors.New("unusable probe input")

// The stage a probe failed at. They separate a path that never opened from one
// that opened and then broke.
var (
	ErrDial    = errors.New("dial")
	ErrRequest = errors.New("do request")
	// ErrResponseBody means the response arrived and its body then failed
	// partway, so the target was reached.
	ErrResponseBody = errors.New("read response body")
)

// Result reports one completed probe. Its durations span the handshake and the
// request, plus the dial for an outbound that does not defer its handshake to
// first use.
type Result struct {
	HTTPStatus int
	// TimeToHeaders spans the request up to the response headers.
	TimeToHeaders time.Duration
	// Elapsed additionally spans reading the body.
	Elapsed time.Duration
	// BytesRead counts body bytes, stopping at the limit Measure was given.
	BytesRead int64
	// ThroughputBytesPerSecond is BytesRead over the time spent reading them,
	// and is zero when nothing was read.
	ThroughputBytesPerSecond float64
}

// Run dials out, completes an HTTP GET to probeURL over that one connection,
// discarding the body, and reports how long it took. The status the target
// returned is not an error; a body that fails partway is, as ErrResponseBody.
// The connection is closed before Run returns.
//
// The reported duration spans the handshake and the request, plus the dial for
// an outbound that does not defer its handshake to first use. It is reported
// alongside an error too, so a caller that tolerates a stage still has its
// measurement.
func Run(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration) (time.Duration, error) {
	result, err := Measure(ctx, out, probeURL, timeout, 0)
	return result.Elapsed, err
}

// Measure is Run with the full record of what the probe observed, reading at
// most maxBytes of the response body; a non-positive maxBytes reads all of it.
// A returned error carries the stage it failed at, and the result holds
// whatever had been observed by then.
func Measure(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration, maxBytes int64) (Result, error) {
	var result Result
	if probeURL == "" {
		return result, fmt.Errorf("%w: empty probe URL", ErrUnusableInput)
	}
	if timeout <= 0 {
		return result, fmt.Errorf("%w: non-positive timeout %s", ErrUnusableInput, timeout)
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrUnusableInput, err)
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
		return result, fmt.Errorf("%w: no host or port in probe URL %q", ErrUnusableInput, probeURL)
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := out.DialContext(probeCtx, "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
	if err != nil {
		result.Elapsed = time.Since(start)
		return result, fmt.Errorf("%w: %w", ErrDial, err)
	}
	defer conn.Close()
	if earlyConn, ok := common.Cast[N.EarlyConn](conn); ok && earlyConn.NeedHandshake() {
		start = time.Now()
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return result, fmt.Errorf("new request: %w", err)
	}
	if tp := linkURL.Query().Get("tp"); tp != "" {
		req.Header.Set("traceparent", tp)
	}

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
