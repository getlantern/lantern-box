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

	"github.com/sagernet/sing-box/common/mux"
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
//
// A stream of an inbound multiplex session has no socket of its own; the
// session's socket stands in for it. That socket is shared, so it is judged by
// how much of it the client acknowledged after the probe began rather than by
// how much is outstanding, and its deadlines are left alone, since they would
// cut every stream of the session.
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
	shared := false
	if !isTCP {
		if sc, ok := mux.SessionConnFromContext(ctx); ok {
			tc, isTCP = common.Cast[*net.TCPConn](sc)
			shared = isTCP
		}
	}
	if !isTCP {
		return r.awaitWrite(ctx, conn, r.writeAsync(ctx, conn, payload), start, writeDeadline, nil)
	}

	// The write runs alongside the poll because a body larger than the free send
	// buffer blocks Write until the peer acknowledges, which on a frozen flow is
	// never; polling only afterwards would turn every stall into a full max_wait.
	// Deadlines also go on the raw socket so a wrapper that ignores them cannot
	// leave the write blocked forever.
	var baseline sendState
	if shared {
		// The client may acknowledge some of the response before the first
		// poll, so a shared socket's baseline is taken before writing.
		var err error
		if baseline, err = r.readState(tc); err != nil {
			return r.awaitWrite(ctx, conn, r.writeAsync(ctx, conn, payload), start, writeDeadline, nil)
		}
	} else {
		tc.SetWriteDeadline(writeDeadline)
	}
	written := r.writeAsync(ctx, conn, payload)
	writeDone := false
	// abortWrite unblocks an outstanding write and returns its result.
	abortWrite := func() error {
		if writeDone {
			return nil
		}
		conn.SetWriteDeadline(time.Now())
		if shared {
			return r.abortStreamWrite(conn, written)
		}
		tc.SetWriteDeadline(time.Now())
		return (<-written).err
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
		// Without socket state only the write result is known. A shared
		// socket's deadlines stay untouched.
		raw := tc
		if shared {
			raw = nil
		}
		return r.awaitWrite(ctx, conn, written, start, writeDeadline, raw)
	}
	// watermark is, on a shared socket, the socket's byte count once the
	// response had been written to it: everything queued ahead of it, other
	// streams' bytes included.
	var watermark uint64
	lastAcked, lastProgress := st.acked, r.now()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if !writeDone {
			select {
			case res := <-written:
				err := res.err
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
				watermark = st.acked + uint64(st.unacked)
			default:
			}
		}
		// Delivered means nothing written is still unacknowledged; unacked
		// alone is checked because a sum with acked comes from two reads.
		//
		// On a shared socket other streams' bytes keep unacked from settling,
		// so delivery is the client acknowledging up to the watermark, which
		// readSendState's read order can only overstate: TCP
		// delivers in order, so that covers the response and everything
		// queued ahead of it. smux and yamux return from Write only once the
		// frame reached the socket, but h2mux can still be holding the tail in
		// its HTTP/2 writer, so the client must also have acknowledged at least
		// the response's length since the baseline.
		if writeDone && deliveredOn(shared, st, baseline, watermark, len(payload)) {
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
func (r *responder) awaitWrite(ctx context.Context, conn net.Conn, written <-chan writeResult, start, writeDeadline time.Time, raw *net.TCPConn) result {
	setDeadline := func(t time.Time) {
		conn.SetWriteDeadline(t)
		if raw != nil {
			raw.SetWriteDeadline(t)
		}
	}
	// verdictFor classifies a write that returned by when it returned, not
	// when this goroutine got to see it: the result and the timer can be ready
	// together, and select picks either.
	verdictFor := func(res writeResult) result {
		switch {
		case errors.Is(res.err, errWritePanic):
			return result{verdict: verdictAborted, drain: r.now().Sub(start)}
		case res.err != nil, !res.done.Before(writeDeadline):
			return result{verdict: verdictStalled, drain: r.now().Sub(start)}
		default:
			return result{verdict: verdictUnknown, drain: r.now().Sub(start)}
		}
	}
	// abort unblocks the write and waits up to writeAbortGrace for it,
	// reporting whether it returned and with what.
	abort := func() (writeResult, bool) {
		setDeadline(time.Now())
		conn.Close()
		grace := time.NewTimer(writeAbortGrace)
		defer grace.Stop()
		select {
		case res := <-written:
			return res, true
		case <-grace.C:
			return writeResult{}, false
		}
	}
	timer := time.NewTimer(time.Until(writeDeadline))
	defer timer.Stop()
	select {
	case res := <-written:
		return verdictFor(res)
	case <-ctx.Done():
		abort()
		return result{verdict: verdictAborted, drain: r.now().Sub(start)}
	case <-timer.C:
	}
	res, returned := abort()
	if !returned {
		return result{verdict: verdictStalled, drain: r.now().Sub(start)}
	}
	return verdictFor(res)
}

// deliveredOn reports whether st shows a fully written response delivered;
// see respond.
func deliveredOn(shared bool, st, baseline sendState, watermark uint64, size int) bool {
	if !shared {
		return st.unacked == 0
	}
	// The watermark can overstate what was written, so an idle session that
	// never acknowledges up to it is delivered once it has fully drained.
	return st.acked-baseline.acked >= uint64(size) && (st.acked >= watermark || st.unacked == 0)
}

// abortStreamWrite waits for a write on a multiplexed stream whose write
// deadline has just been set to now. A stream that ignores the deadline is
// closed, which ends only that stream; one that doesn't return even then is
// abandoned after writeAbortGrace, as awaitWrite does, so the probe can't hang.
func (r *responder) abortStreamWrite(conn net.Conn, written <-chan writeResult) error {
	grace := time.NewTimer(writeAbortGrace)
	defer grace.Stop()
	select {
	case res := <-written:
		return res.err
	case <-grace.C:
	}
	conn.Close()
	grace.Reset(writeAbortGrace)
	select {
	case res := <-written:
		return res.err
	case <-grace.C:
		return errWriteAbandoned
	}
}

// errWriteAbandoned marks a probe write that never returned, even after its
// stream was closed.
var errWriteAbandoned = errors.New("banditprobe: probe write did not return")

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

// writeResult is how a probe response write ended and when it returned.
type writeResult struct {
	err  error
	done time.Time
}

// writeAsync writes payload on its own goroutine and delivers the result on
// the returned channel. recover only covers the goroutine it runs in, so a
// panic in a wrapper's Write is caught here, logged, and delivered as
// errWritePanic rather than taking down the proxy.
func (r *responder) writeAsync(ctx context.Context, conn net.Conn, payload []byte) <-chan writeResult {
	written := make(chan writeResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				err := fmt.Errorf("%w: %v", errWritePanic, p)
				r.logger.ErrorContext(ctx, err)
				written <- writeResult{err: err, done: time.Now()}
			}
		}()
		var err error
		// Some inbounds frame each Write as a single protocol chunk and panic
		// instead of splitting one larger than their maximum (shadowsocks 2022
		// caps a chunk at 64 KiB - 1, less than the header plus the body).
		for len(payload) > 0 && err == nil {
			var n int
			n, err = conn.Write(payload[:min(len(payload), maxWriteChunk)])
			if n == 0 && err == nil {
				err = io.ErrShortWrite
			}
			payload = payload[n:]
		}
		written <- writeResult{err: err, done: time.Now()}
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
