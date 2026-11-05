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
// the end of the response body: one round trip through the outbound to the
// target. Connection setup (the dial, any proxy or TLS handshake, and a
// handshake the outbound defers to the first write) is excluded, so outbounds
// that establish connections differently are timed alike.
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

	var start time.Time
	traceCtx := httptrace.WithClientTrace(probeCtx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { start = time.Now() },
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
		return 0, fmt.Errorf("do request: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return time.Since(start), nil
}
