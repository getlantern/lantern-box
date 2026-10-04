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
	// sing's formatter panics on types it doesn't know, a named string type
	// included, so the verdict is logged as a plain string.
	r.logger.DebugContext(ctx, "bandit probe ", string(res.verdict), " drain=", res.drain, " acked=", res.acked,
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
		return r.awaitWrite(ctx, conn, r.writeAsync(ctx, conn, payload), start, writeDeadline, nil)
	}

	// The write runs alongside the poll because a body larger than the free send
	// buffer blocks Write until the peer acknowledges, which on a frozen flow is
	// never; polling only afterwards would turn every stall into a full max_wait.
	// Deadlines also go on the raw socket so a wrapper that ignores them cannot
	// leave the write blocked forever.
	tc.SetWriteDeadline(writeDeadline)
	written := r.writeAsync(ctx, conn, payload)
	writeDone := false
	// abortWrite unblocks an outstanding write and returns its result.
	abortWrite := func() error {
		if writeDone {
			return nil
		}
		conn.SetWriteDeadline(time.Now())
		tc.SetWriteDeadline(time.Now())
		return <-written
	}
	stalled := func(st sendState) result {
		// The write can panic after the loop last checked it; that is still a
		// bug here, not a stalled route.
		if errors.Is(abortWrite(), errWritePanic) {
			return result{verdict: verdictAborted, drain: r.now().Sub(start), acked: st.acked, state: st}
		}
		return result{verdict: verdictStalled, drain: r.now().Sub(start), acked: st.acked, state: st}
	}

	st, err := r.readState(tc)
	if err != nil {
		// Without socket state only the write result is known.
		return r.awaitWrite(ctx, conn, written, start, writeDeadline, tc)
	}
	lastAcked, lastProgress := st.acked, r.now()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if !writeDone {
			select {
			case err := <-written:
				if errors.Is(err, errWritePanic) {
					return result{verdict: verdictAborted, drain: r.now().Sub(start), acked: st.acked, state: st}
				}
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
		// With nothing outstanding the flow isn't frozen, even if a wrapper is
		// still holding Write open after the bytes reached the socket.
		if st.acked != lastAcked || st.unacked == 0 {
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

// awaitWrite decides a probe from the write result alone, for a conn whose
// socket state can't be read. unknown is reported as a success, so it requires
// the whole response to have been written by max_wait; anything less is a
// stall. raw, when set, is the kernel socket under conn and gets the same
// deadline.
//
// The deadline is enforced here rather than left to the conn because some
// wrappers ignore SetWriteDeadline (samizdat's HTTP/2 stream conn is a no-op),
// and their Write blocks for as long as the peer withholds flow-control
// credit. Past max_wait the conn is closed to unblock such a write, and if even
// that doesn't return it within writeAbortGrace the probe is called stalled
// without it.
func (r *responder) awaitWrite(ctx context.Context, conn net.Conn, written <-chan error, start, writeDeadline time.Time, raw *net.TCPConn) result {
	setDeadline := func(t time.Time) {
		conn.SetWriteDeadline(t)
		if raw != nil {
			raw.SetWriteDeadline(t)
		}
	}
	verdictFor := func(err error, ifWritten verdict) result {
		switch {
		case errors.Is(err, errWritePanic):
			return result{verdict: verdictAborted, drain: r.now().Sub(start)}
		case err != nil:
			return result{verdict: verdictStalled, drain: r.now().Sub(start)}
		default:
			return result{verdict: ifWritten, drain: r.now().Sub(start)}
		}
	}
	// abort unblocks the write and waits up to writeAbortGrace for it,
	// reporting whether it returned and with what.
	abort := func() (bool, error) {
		setDeadline(time.Now())
		conn.Close()
		grace := time.NewTimer(writeAbortGrace)
		defer grace.Stop()
		select {
		case err := <-written:
			return true, err
		case <-grace.C:
			return false, nil
		}
	}
	timer := time.NewTimer(time.Until(writeDeadline))
	defer timer.Stop()
	select {
	case err := <-written:
		// A write and the timer can be ready together, and select picks
		// either; a write that only returned at max_wait wasn't delivered in
		// time.
		if !time.Now().Before(writeDeadline) {
			return verdictFor(err, verdictStalled)
		}
		return verdictFor(err, verdictUnknown)
	case <-ctx.Done():
		abort()
		return result{verdict: verdictAborted, drain: r.now().Sub(start)}
	case <-timer.C:
	}
	returned, err := abort()
	if !returned {
		return result{verdict: verdictStalled, drain: r.now().Sub(start)}
	}
	// The write can have finished just as max_wait passed; it was not
	// delivered in time either way.
	return verdictFor(err, verdictStalled)
}

// writeAbortGrace bounds how long a probe waits for a write to return after
// its conn was closed at max_wait.
const writeAbortGrace = time.Second

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

// errWritePanic marks a probe response write that panicked. It is a bug on
// this side rather than a sign the route stalled, so it ends the probe as
// aborted and is never reported.
var errWritePanic = errors.New("banditprobe: panic writing probe response")

// writeAsync writes payload on its own goroutine and delivers the result on
// the returned channel. recover only covers the goroutine it runs in, so a
// panic in a wrapper's Write is caught here, logged, and delivered as
// errWritePanic rather than taking down the proxy.
func (r *responder) writeAsync(ctx context.Context, conn net.Conn, payload []byte) <-chan error {
	written := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				err := fmt.Errorf("%w: %v", errWritePanic, p)
				r.logger.ErrorContext(ctx, err)
				written <- err
			}
		}()
		_, err := conn.Write(payload)
		written <- err
	}()
	return written
}

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
