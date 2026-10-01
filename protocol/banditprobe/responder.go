package banditprobe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/logger"
)

type verdict string

const (
	verdictDelivered verdict = "delivered"
	verdictStalled   verdict = "stalled"
	// verdictUnknown means the client-facing socket is not a kernel TCP socket
	// (a multiplexed stream or a QUIC inbound), so delivery was not observed.
	verdictUnknown verdict = "unknown"
	// verdictAborted means the proxy gave up for its own reasons, such as
	// shutting down; it says nothing about the route and is never reported.
	verdictAborted verdict = "aborted"
)

type config struct {
	callbackURL   *url.URL
	bodySize      int
	stallTimeout  time.Duration
	maxWait       time.Duration
	reportStalled bool
}

type result struct {
	verdict verdict
	drain   time.Duration
	acked   uint64
	state   sendState
}

type responder struct {
	cfg        config
	logger     logger.ContextLogger
	pool       []byte
	readState  func(*net.TCPConn) (sendState, error)
	httpClient *http.Client
	now        func() time.Time
}

func (r *responder) serve(ctx context.Context, conn net.Conn) error {
	conn.SetReadDeadline(r.now().Add(requestReadTimeout))
	req, err := http.ReadRequest(bufio.NewReader(io.LimitReader(conn, maxRequestBytes)))
	if err != nil {
		return fmt.Errorf("reading probe request: %w", err)
	}
	conn.SetReadDeadline(time.Time{})

	if req.Method != http.MethodGet || req.URL.Path != r.cfg.callbackURL.Path {
		writeStatus(conn, http.StatusNotFound)
		return fmt.Errorf("unexpected probe request %s %s", req.Method, req.URL.Path)
	}
	query := req.URL.Query()
	if query.Get("token") == "" {
		writeStatus(conn, http.StatusBadRequest)
		return errors.New("probe request has no token")
	}

	res := r.respond(ctx, conn)
	conn.Close()
	// Shutdown closes inbound conns, which surfaces as write or socket errors;
	// those say nothing about the route.
	if ctx.Err() != nil {
		res.verdict = verdictAborted
	}
	r.logger.DebugContext(ctx, "bandit probe ", res.verdict, " drain=", res.drain, " acked=", res.acked,
		" retrans=", res.state.retrans, " rtt=", res.state.rtt)
	if res.verdict == verdictAborted || (res.verdict == verdictStalled && !r.cfg.reportStalled) {
		return nil
	}
	if err := r.sendCallback(ctx, query, req.Header.Get("traceparent"), res); err != nil {
		r.logger.WarnContext(ctx, "bandit probe callback failed: ", err)
	}
	return nil
}

// respond writes the probe body and decides whether the client received it.
// Delivery is judged from what the client's kernel acknowledged, never from
// Write returning: a successful Write only means the bytes reached the local
// send buffer, which is exactly what a frozen flow still allows.
func (r *responder) respond(ctx context.Context, conn net.Conn) result {
	start := r.now()
	deadline := start.Add(r.cfg.maxWait)
	writeDeadline := time.Now().Add(r.cfg.maxWait)
	header := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Length: " + strconv.Itoa(r.cfg.bodySize) + "\r\n" +
		"Cache-Control: no-store\r\n" +
		"Connection: close\r\n\r\n"
	payload := append([]byte(header), r.body()...)

	conn.SetWriteDeadline(writeDeadline)
	tc, isTCP := common.Cast[*net.TCPConn](conn)
	if !isTCP {
		written := make(chan error, 1)
		go func() {
			_, err := conn.Write(payload)
			written <- err
		}()
		select {
		case err := <-written:
			if err != nil {
				return result{verdict: verdictStalled, drain: r.now().Sub(start)}
			}
			return result{verdict: verdictUnknown, drain: r.now().Sub(start)}
		case <-ctx.Done():
			conn.SetWriteDeadline(time.Now())
			<-written
			return result{verdict: verdictAborted, drain: r.now().Sub(start)}
		}
	}

	// The write runs alongside the poll because a body larger than the free send
	// buffer blocks Write until the peer acknowledges, which on a frozen flow is
	// never; polling only afterwards would turn every stall into a full max_wait.
	// Deadlines also go on the raw socket so a wrapper that ignores them cannot
	// leave the write blocked forever.
	tc.SetWriteDeadline(writeDeadline)
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		written <- err
	}()
	writeDone := false
	abortWrite := func() {
		if !writeDone {
			conn.SetWriteDeadline(time.Now())
			tc.SetWriteDeadline(time.Now())
			<-written
		}
	}
	stalled := func(st sendState) result {
		abortWrite()
		return result{verdict: verdictStalled, drain: r.now().Sub(start), acked: st.acked, state: st}
	}

	st, err := r.readState(tc)
	if err != nil {
		// Without socket state only the write result is known, and unknown is
		// reported as a success, so it requires the whole response to have been
		// written.
		select {
		case err := <-written:
			if err != nil {
				return result{verdict: verdictStalled, drain: r.now().Sub(start)}
			}
			return result{verdict: verdictUnknown, drain: r.now().Sub(start)}
		case <-ctx.Done():
			abortWrite()
			return result{verdict: verdictAborted, drain: r.now().Sub(start)}
		}
	}
	lastAcked, lastProgress := st.acked, r.now()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if !writeDone {
			select {
			case err := <-written:
				if err != nil {
					return result{verdict: verdictStalled, drain: r.now().Sub(start), acked: st.acked, state: st}
				}
				writeDone = true
				next, err := r.readState(tc)
				if err != nil {
					return stalled(st)
				}
				st = next
			default:
			}
		}
		// Delivered means nothing written is still unacknowledged. acked and
		// unacked come from separate syscalls, so a target summed from them can
		// fall short of the response's end; unacked alone cannot.
		if writeDone && st.unacked == 0 {
			return result{verdict: verdictDelivered, drain: r.now().Sub(start), acked: st.acked, state: st}
		}
		now := r.now()
		if st.acked != lastAcked {
			lastAcked, lastProgress = st.acked, now
		}
		if now.Sub(lastProgress) >= r.cfg.stallTimeout || !now.Before(deadline) {
			return stalled(st)
		}
		select {
		case <-ctx.Done():
			abortWrite()
			return result{verdict: verdictAborted, drain: r.now().Sub(start), acked: st.acked, state: st}
		case <-ticker.C:
		}
		next, err := r.readState(tc)
		if err != nil {
			return stalled(st)
		}
		st = next
	}
}

const pollInterval = 50 * time.Millisecond

// body returns bodySize random bytes from a random offset in the pool, so the
// response is incompressible and differs from probe to probe.
func (r *responder) body() []byte {
	offset := rand.IntN(len(r.pool) - r.cfg.bodySize + 1)
	return r.pool[offset : offset+r.cfg.bodySize]
}

// reservedParams are set only by the proxy; a client copy is discarded so it
// cannot pre-empt or contradict the proxy's own observation.
var reservedParams = []string{"verdict", "drain_ms", "acked", "retrans", "rtt_ms"}

// sendCallback forwards the client's callback to the API with the verdict
// attached. Client parameters other than the reserved and configured ones are
// passed through, so the API's probe lookup and latency accounting still
// apply; drain_ms lets it take the delivery wait out of that latency.
func (r *responder) sendCallback(ctx context.Context, clientQuery url.Values, traceparent string, res result) error {
	u := *r.cfg.callbackURL
	q := url.Values{}
	for k, vs := range clientQuery {
		q[k] = append([]string(nil), vs...)
	}
	for _, k := range reservedParams {
		q.Del(k)
	}
	for k, vs := range r.cfg.callbackURL.Query() {
		q[k] = append([]string(nil), vs...)
	}
	q.Set("verdict", string(res.verdict))
	q.Set("drain_ms", strconv.FormatInt(res.drain.Milliseconds(), 10))
	if res.verdict != verdictUnknown {
		q.Set("acked", strconv.FormatUint(res.acked, 10))
		q.Set("retrans", strconv.FormatUint(uint64(res.state.retrans), 10))
		q.Set("rtt_ms", strconv.FormatInt(res.state.rtt.Milliseconds(), 10))
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if traceparent == "" {
		traceparent = clientQuery.Get("tp")
	}
	if traceparent != "" {
		req.Header.Set("traceparent", traceparent)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("callback returned %s", resp.Status)
	}
	return nil
}

func writeStatus(conn net.Conn, code int) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, http.StatusText(code))
}
