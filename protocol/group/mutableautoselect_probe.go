package group

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/ntp"
)

type probeResult struct {
	tag     string
	success bool
	delayMs uint32
}

// runProbe issues an HTTP GET through out to probeURL under the
// per-protocol timeout. Success implies a completed handshake; for
// bandit-supplied URLs it also implies the callback handler received the
// request.
func runProbe(
	ctx context.Context,
	out A.Outbound,
	probeURL string,
	beh protocolBehavior,
) probeResult {
	tag := out.Tag()
	if beh.excludeFromPool || probeURL == "" {
		return probeResult{tag: tag}
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return probeResult{tag: tag}
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

	probeCtx, cancel := context.WithTimeout(ctx, beh.probeTimeout)
	defer cancel()

	start := time.Now()
	conn, err := out.DialContext(probeCtx, "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
	if err != nil {
		return probeResult{tag: tag}
	}
	defer conn.Close()
	if earlyConn, ok := common.Cast[N.EarlyConn](conn); ok && earlyConn.NeedHandshake() {
		start = time.Now()
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return probeResult{tag: tag}
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
		return probeResult{tag: tag}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 1ms floor so a sub-millisecond probe isn't reported as 0; rank
	// treats delay==0 as "no recent success" and would drop the winner.
	delayMs := uint32(time.Since(start) / time.Millisecond)
	if delayMs == 0 {
		delayMs = 1
	}
	return probeResult{tag: tag, success: true, delayMs: delayMs}
}

// probeAll runs jobs with up to probeConcurrency workers, records each
// outcome, and calls onSuccess serially for successful probes. It returns
// after all queued probes complete or ctx is canceled.
func (s *MutableAutoSelect) probeAll(
	ctx context.Context,
	jobs []probeJob,
	onSuccess func(res probeResult),
) {
	if len(jobs) == 0 {
		return
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		queue   = make(chan probeJob)
		workers = max(1, min(len(jobs), s.cfg.probeConcurrency))
	)
	for range workers {
		wg.Go(func() {
			for j := range queue {
				res := runProbe(ctx, j.outbound, j.probeURL, j.beh)
				// Batch cancellation (shutdown or ladder budget) is not member
				// evidence. Per-probe timeouts use a child context, so the
				// batch ctx remains live and the failure still counts.
				if ctx.Err() != nil {
					continue
				}
				s.recordProbeOutcome(res.tag, res.success, res.delayMs)
				if !res.success || onSuccess == nil {
					continue
				}
				mu.Lock()
				onSuccess(res)
				mu.Unlock()
			}
		})
	}
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return
		}
	}
	close(queue)
	wg.Wait()
}
