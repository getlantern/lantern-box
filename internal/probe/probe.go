// Package probe dials an outbound and completes one HTTP request through it,
// timed.
package probe

import (
	"context"
	"crypto/tls"
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

// Result is one attempt's outcome. Delay is meaningful only when Success: it
// spans the handshake and the request, plus the dial for an outbound that does
// not defer its handshake to first use.
type Result struct {
	Success bool
	Delay   time.Duration
}

// Run dials out and completes an HTTP GET to probeURL over that one
// connection, discarding the body. Success means the request completed,
// whatever status it returned; when probeURL's handler confirms receipt, it
// also means the handler was reached.
//
// timeout bounds the whole attempt: a non-positive timeout, or a probeURL that
// is empty or unparsable, fails without dialing. The connection is closed
// before Run returns.
func Run(ctx context.Context, out A.Outbound, probeURL string, timeout time.Duration) Result {
	if probeURL == "" || timeout <= 0 {
		return Result{}
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return Result{}
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

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	conn, err := out.DialContext(probeCtx, "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
	if err != nil {
		return Result{}
	}
	defer conn.Close()
	if earlyConn, ok := common.Cast[N.EarlyConn](conn); ok && earlyConn.NeedHandshake() {
		start = time.Now()
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return Result{}
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
		return Result{}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return Result{Success: true, Delay: time.Since(start)}
}
