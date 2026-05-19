package group

import (
	"context"
	"crypto/tls"
	"errors"
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
	outcome localOutcome
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
		return probeResult{tag: tag, outcome: outcomeHandshakeFailure}
	}
	linkURL, err := url.Parse(probeURL)
	if err != nil {
		return probeResult{tag: tag, outcome: outcomeHandshakeFailure}
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
		return probeResult{tag: tag, outcome: classifyDialError(probeCtx, err)}
	}
	defer conn.Close()
	if earlyConn, ok := common.Cast[N.EarlyConn](conn); ok && earlyConn.NeedHandshake() {
		start = time.Now()
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return probeResult{tag: tag, outcome: outcomeHandshakeFailure}
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
		return probeResult{tag: tag, outcome: classifyDialError(probeCtx, err)}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// 1ms floor so a sub-millisecond probe isn't reported as 0; rank
	// treats delay==0 as "no recent success" and would drop the winner.
	delayMs := uint32(time.Since(start) / time.Millisecond)
	if delayMs == 0 {
		delayMs = 1
	}
	return probeResult{tag: tag, outcome: outcomeSuccess, delayMs: delayMs}
}

func classifyDialError(ctx context.Context, err error) localOutcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return outcomeTimeout
	}
	return outcomeHandshakeFailure
}

// probeAll fans out one probe per job over up to probeConcurrency
// goroutines, recording each result via recordOutcome and streaming
// successes through onSuccess (called serialized). Returns when every
// probe completes or ctx fires.
func (s *MutableAutoSelect) probeAll(
	ctx context.Context,
	jobs []probeJob,
	onSuccess func(res probeResult),
) {
	if len(jobs) == 0 {
		return
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, probeConcurrency)
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j probeJob) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			res := runProbe(ctx, j.outbound, j.probeURL, j.beh)
			s.recordOutcome(res.tag, res.outcome, res.delayMs)
			if res.outcome != outcomeSuccess || onSuccess == nil {
				return
			}
			mu.Lock()
			onSuccess(res)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
}
