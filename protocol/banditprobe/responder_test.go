package banditprobe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

const testBodySize = 4096

type callbackRecorder struct {
	mu      sync.Mutex
	queries []url.Values
	headers []http.Header
	srv     *httptest.Server
}

func newCallbackRecorder(t *testing.T) *callbackRecorder {
	rec := &callbackRecorder{}
	rec.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.queries = append(rec.queries, r.URL.Query())
		rec.headers = append(rec.headers, r.Header.Clone())
		rec.mu.Unlock()
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (c *callbackRecorder) calls() []url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]url.Values(nil), c.queries...)
}

// scriptedState returns states in order, repeating the last one.
func scriptedState(states ...sendState) func(*net.TCPConn) (sendState, error) {
	var mu sync.Mutex
	i := 0
	return func(*net.TCPConn) (sendState, error) {
		mu.Lock()
		defer mu.Unlock()
		st := states[min(i, len(states)-1)]
		i++
		return st, nil
	}
}

func newTestResponder(t *testing.T, rec *callbackRecorder, opts option.BanditProbeOutboundOptions, readState func(*net.TCPConn) (sendState, error)) *responder {
	t.Helper()
	if opts.CallbackURL == "" {
		opts.CallbackURL = rec.srv.URL + "/v1/bandit/callback"
	}
	if opts.BodySize == 0 {
		opts.BodySize = testBodySize
	}
	if opts.StallTimeout == 0 {
		opts.StallTimeout = badoption.Duration(200 * time.Millisecond)
	}
	if opts.MaxWait == 0 {
		opts.MaxWait = badoption.Duration(2 * time.Second)
	}
	cfg, err := newConfig(opts)
	require.NoError(t, err)
	return &responder{
		cfg:        cfg,
		logger:     log.NewNOPFactory().Logger(),
		pool:       make([]byte, bodyPoolSize),
		readState:  readState,
		httpClient: rec.srv.Client(),
		now:        time.Now,
	}
}

// tcpPair returns the server and client ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	server := <-accepted
	require.NotNil(t, server)
	t.Cleanup(func() { client.Close(); server.Close() })
	return server.(*net.TCPConn), client
}

// probe sends a client probe request and returns the parsed response with its
// body fully read.
func probe(t *testing.T, client net.Conn, target string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	require.NoError(t, req.Write(client))
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func runProbe(t *testing.T, r *responder, server net.Conn, client net.Conn, target string) (*http.Response, []byte, error) {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		err := r.serve(context.Background(), server)
		server.Close()
		errc <- err
	}()
	resp, body := probe(t, client, target)
	return resp, body, <-errc
}

const probeTarget = "http://api.example.test/v1/bandit/callback?token=tok-1&tp=tp-1&did=dev-1&cd=40"

func TestServe_DeliveredForwardsCallbackWithClientParams(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, scriptedState(
		sendState{acked: 100, unacked: 4000},
		sendState{acked: 2100, unacked: 2000},
		sendState{acked: 4100, unacked: 0, retrans: 1, rtt: 42 * time.Millisecond},
	))
	server, client := tcpPair(t)

	resp, body, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, testBodySize)

	calls := rec.calls()
	require.Len(t, calls, 1)
	q := calls[0]
	assert.Equal(t, "tok-1", q.Get("token"))
	assert.Equal(t, "tp-1", q.Get("tp"))
	assert.Equal(t, "dev-1", q.Get("did"))
	assert.Equal(t, "40", q.Get("cd"))
	assert.Equal(t, "delivered", q.Get("verdict"))
	assert.Equal(t, "4100", q.Get("acked"))
	assert.Equal(t, "1", q.Get("retrans"))
	assert.Equal(t, "42", q.Get("rtt_ms"))
	assert.NotEmpty(t, q.Get("drain_ms"))
	assert.Equal(t, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", rec.headers[0].Get("traceparent"))
}

func TestServe_StalledDropsCallbackByDefault(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, scriptedState(
		sendState{acked: 100, unacked: 4000},
		sendState{acked: 1500, unacked: 2600},
	))
	server, client := tcpPair(t)

	start := time.Now()
	_, _, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Duration(r.cfg.maxWait), "a stall should be called before max_wait")
	assert.Empty(t, rec.calls(), "report_stalled is off, so a stalled probe sends no callback")
}

func TestServe_StalledReportedWhenEnabled(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 100, unacked: 4000},
		sendState{acked: 1500, unacked: 2600, retrans: 7},
	))
	server, client := tcpPair(t)

	_, _, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "stalled", calls[0].Get("verdict"))
	assert.Equal(t, "1500", calls[0].Get("acked"))
	assert.Equal(t, "7", calls[0].Get("retrans"))
}

func TestServe_CancelledProbeIsNotReported(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		ReportStalled: true,
		StallTimeout:  badoption.Duration(maxMaxWait),
		MaxWait:       badoption.Duration(maxMaxWait),
	}, scriptedState(sendState{acked: 100, unacked: 4000}))
	server, client := tcpPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.serve(ctx, server) }()
	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))
	go io.Copy(io.Discard, client)
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve ignored cancellation")
	}
	assert.Empty(t, rec.calls(), "a probe cut short by the proxy says nothing about the route")
}

// blockingConn is a non-TCP conn whose Write blocks until its write deadline
// passes, like a stream whose peer has stopped reading.
type blockingConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	changed  chan struct{}
}

func newBlockingConn(c net.Conn) *blockingConn {
	return &blockingConn{Conn: c, changed: make(chan struct{}, 1)}
}

func (c *blockingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return nil
}

func (c *blockingConn) Write(b []byte) (int, error) {
	for {
		c.mu.Lock()
		d := c.deadline
		c.mu.Unlock()
		if !d.IsZero() && !time.Now().Before(d) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-c.changed:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestServe_CancelledNonTCPProbeIsAborted(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		ReportStalled: true,
		MaxWait:       badoption.Duration(maxMaxWait),
	}, nil)
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.serve(ctx, newBlockingConn(server)) }()
	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked non-TCP write ignored cancellation")
	}
	assert.Empty(t, rec.calls())
}

// deadlineIgnoringConn is a non-TCP conn whose Write ignores write deadlines
// and blocks until the conn is closed, like samizdat's HTTP/2 stream conn
// while the peer withholds flow-control credit. With ignoreClose set, Close
// doesn't unblock it either.
type deadlineIgnoringConn struct {
	net.Conn
	ignoreClose bool
	closed      chan struct{}
	closeOnce   sync.Once
	release     chan struct{}
}

func newDeadlineIgnoringConn(t *testing.T, c net.Conn, ignoreClose bool) *deadlineIgnoringConn {
	dc := &deadlineIgnoringConn{Conn: c, ignoreClose: ignoreClose, closed: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(dc.release) })
	return dc
}

func (c *deadlineIgnoringConn) SetWriteDeadline(time.Time) error { return nil }

func (c *deadlineIgnoringConn) Write([]byte) (int, error) {
	if c.ignoreClose {
		<-c.release
		return 0, net.ErrClosed
	}
	select {
	case <-c.closed:
	case <-c.release:
	}
	return 0, net.ErrClosed
}

func (c *deadlineIgnoringConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func serveBlocked(t *testing.T, r *responder, ctx context.Context, conn net.Conn, client net.Conn) <-chan error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- r.serve(ctx, conn) }()
	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))
	return errc
}

func TestServe_NonTCPWriteIgnoringDeadlineIsStalledAtMaxWait(t *testing.T) {
	for _, tt := range []struct {
		name        string
		ignoreClose bool
	}{
		{"unblocked by close", false},
		{"not even unblocked by close", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := newCallbackRecorder(t)
			maxWait := 300 * time.Millisecond
			r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
				ReportStalled: true,
				MaxWait:       badoption.Duration(maxWait),
			}, nil)
			server, client := net.Pipe()
			t.Cleanup(func() { client.Close(); server.Close() })

			start := time.Now()
			errc := serveBlocked(t, r, context.Background(), newDeadlineIgnoringConn(t, server, tt.ignoreClose), client)
			select {
			case err := <-errc:
				require.NoError(t, err)
			case <-time.After(maxWait + writeAbortGrace + 3*time.Second):
				t.Fatal("a write that ignores its deadline held the probe past max_wait")
			}
			assert.GreaterOrEqual(t, time.Since(start), maxWait)
			calls := rec.calls()
			require.Len(t, calls, 1)
			assert.Equal(t, "stalled", calls[0].Get("verdict"))
		})
	}
}

// lateWriteConn completes Write successfully, but only once the probe's
// write deadline has passed, ignoring the deadline itself.
type lateWriteConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (c *lateWriteConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deadline.IsZero() {
		c.deadline = t
	}
	return nil
}

func (c *lateWriteConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	d := c.deadline
	c.mu.Unlock()
	time.Sleep(time.Until(d))
	return len(b), nil
}

func TestServe_NonTCPWriteFinishingAtMaxWaitIsStalled(t *testing.T) {
	for i := 0; i < 5; i++ {
		rec := newCallbackRecorder(t)
		r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
			ReportStalled: true,
			MaxWait:       badoption.Duration(100 * time.Millisecond),
		}, nil)
		server, client := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })

		errc := serveBlocked(t, r, context.Background(), &lateWriteConn{Conn: server}, client)
		select {
		case err := <-errc:
			require.NoError(t, err)
		case <-time.After(writeAbortGrace + 3*time.Second):
			t.Fatal("serve did not return")
		}
		calls := rec.calls()
		require.Len(t, calls, 1)
		assert.Equal(t, "stalled", calls[0].Get("verdict"), "a write that only returned at max_wait was not delivered in time")
	}
}

// TestAwaitWrite_ClassifiesByCompletionTime covers a write result that is
// already buffered when max_wait has passed, so select can take either the
// result or the timer. The verdict must follow when the write returned, not
// which case select picked or when the result was read.
func TestAwaitWrite_ClassifiesByCompletionTime(t *testing.T) {
	r := newTestResponder(t, newCallbackRecorder(t), option.BanditProbeOutboundOptions{}, nil)
	for _, tt := range []struct {
		name   string
		offset time.Duration // completion time relative to the deadline
		err    error
		want   verdict
	}{
		{"returned before max_wait", -time.Millisecond, nil, verdictUnknown},
		{"returned at max_wait", 0, nil, verdictStalled},
		{"returned after max_wait", time.Millisecond, nil, verdictStalled},
		{"failed before max_wait", -time.Millisecond, os.ErrDeadlineExceeded, verdictStalled},
		{"panicked", -time.Millisecond, errWritePanic, verdictAborted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Repeat so both select cases are exercised.
			for i := 0; i < 50; i++ {
				server, client := net.Pipe()
				deadline := time.Now().Add(-time.Millisecond)
				written := make(chan writeResult, 1)
				written <- writeResult{err: tt.err, done: deadline.Add(tt.offset)}
				res := r.awaitWrite(context.Background(), server, written, time.Now(), deadline, nil)
				client.Close()
				require.Equal(t, tt.want, res.verdict)
			}
		})
	}
}

func TestServe_CancelledProbeWithDeadlineIgnoringWriteIsAborted(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		ReportStalled: true,
		MaxWait:       badoption.Duration(maxMaxWait),
	}, nil)
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	errc := serveBlocked(t, r, ctx, newDeadlineIgnoringConn(t, server, true), client)
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(writeAbortGrace + 3*time.Second):
		t.Fatal("cancellation waited on a write that ignores deadlines and close")
	}
	assert.Empty(t, rec.calls())
}

// TestServe_InconsistentSnapshotIsNotDelivered covers acks landing between the
// two socket-state syscalls: acked then lags unacked, and a target built from
// both would be reached while the tail is still outstanding.
func TestServe_InconsistentSnapshotIsNotDelivered(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 0, unacked: 0},
		sendState{acked: 100, unacked: 500},
		sendState{acked: 600, unacked: 400},
	))
	server, client := tcpPair(t)

	_, _, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "stalled", calls[0].Get("verdict"))
}

// blockingTCPConn exposes a real *net.TCPConn through Upstream, as inbound
// wrappers do, while its own Write blocks until its write deadline.
type blockingTCPConn struct {
	*blockingConn
	tcp *net.TCPConn
}

func (c *blockingTCPConn) Upstream() any { return c.tcp }

func TestServe_FailedWriteWithoutSocketStateIsStalled(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		ReportStalled: true,
		MaxWait:       badoption.Duration(300 * time.Millisecond),
	}, func(*net.TCPConn) (sendState, error) {
		return sendState{}, errors.New("tcp send state unavailable")
	})
	server, client := tcpPair(t)
	conn := &blockingTCPConn{blockingConn: newBlockingConn(server), tcp: server}

	errc := make(chan error, 1)
	go func() { errc <- r.serve(context.Background(), conn) }()
	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "stalled", calls[0].Get("verdict"), "a write that never completed must not be reported as unknown")
}

func TestServe_CompletedWriteWithoutSocketStateIsUnknown(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, func(*net.TCPConn) (sendState, error) {
		return sendState{}, errors.New("tcp send state unavailable")
	})
	server, client := tcpPair(t)

	resp, body, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, testBodySize)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "unknown", calls[0].Get("verdict"))
}

// throttledTCPConn hands bytes to the socket at once and then holds Write
// open, the way a post-write rate limiter does.
type throttledTCPConn struct {
	*net.TCPConn
	hold time.Duration
}

func (c *throttledTCPConn) Write(b []byte) (int, error) {
	n, err := c.TCPConn.Write(b)
	time.Sleep(c.hold)
	return n, err
}

func (c *throttledTCPConn) Upstream() any { return c.TCPConn }

func TestServe_PostWriteThrottleIsNotAStall(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
		sendState{acked: 4100, unacked: 0},
	))
	server, client := tcpPair(t)
	conn := &throttledTCPConn{TCPConn: server, hold: 3 * time.Duration(r.cfg.stallTimeout)}

	errc := make(chan error, 1)
	go func() { errc <- r.serve(context.Background(), conn) }()
	_, body := probe(t, client, probeTarget)
	assert.Len(t, body, testBodySize)
	require.NoError(t, <-errc)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "delivered", calls[0].Get("verdict"), "an acknowledged body must not stall while Write is held open")
}

func TestServe_MaxWaitBoundsSlowProgress(t *testing.T) {
	rec := newCallbackRecorder(t)
	var mu sync.Mutex
	acked := uint64(100)
	// Acks keep trickling in, so the stall timeout never fires.
	trickle := func(*net.TCPConn) (sendState, error) {
		mu.Lock()
		defer mu.Unlock()
		acked++
		return sendState{acked: acked, unacked: 1 << 20}, nil
	}
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		ReportStalled: true,
		MaxWait:       badoption.Duration(300 * time.Millisecond),
	}, trickle)
	server, client := tcpPair(t)

	_, _, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "stalled", calls[0].Get("verdict"))
}

func TestServe_SendStateErrorIsStalled(t *testing.T) {
	rec := newCallbackRecorder(t)
	calls := 0
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true}, func(*net.TCPConn) (sendState, error) {
		calls++
		if calls == 1 {
			return sendState{acked: 100, unacked: 4000}, nil
		}
		return sendState{}, errors.New("connection reset")
	})
	server, client := tcpPair(t)

	_, _, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	got := rec.calls()
	require.Len(t, got, 1)
	assert.Equal(t, "stalled", got[0].Get("verdict"))
}

func TestServe_NonTCPConnReportsUnknown(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, func(*net.TCPConn) (sendState, error) {
		t.Fatal("readState must not be called for a non-TCP conn")
		return sendState{}, nil
	})
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	resp, body, err := runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, testBodySize)
	calls := rec.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "unknown", calls[0].Get("verdict"))
	assert.Empty(t, calls[0].Get("acked"))
}

func TestServe_ClientCannotSpoofProxyParams(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{
		CallbackURL: rec.srv.URL + "/v1/bandit/callback?src=proxy",
	}, nil)
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	_, _, err := runProbe(t, r, server, client,
		"http://api.example.test/v1/bandit/callback?token=tok-1&verdict=delivered&acked=999&retrans=0&rtt_ms=1&drain_ms=0&src=client")
	require.NoError(t, err)
	calls := rec.calls()
	require.Len(t, calls, 1)
	q := calls[0]
	assert.Equal(t, []string{"unknown"}, q["verdict"])
	assert.Empty(t, q["acked"])
	assert.Empty(t, q["retrans"])
	assert.Empty(t, q["rtt_ms"])
	assert.Len(t, q["drain_ms"], 1)
	assert.Equal(t, []string{"proxy"}, q["src"])
	assert.Equal(t, "tok-1", q.Get("token"))
}

func TestServe_RejectsOversizedRequest(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, nil)
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	errc := make(chan error, 1)
	go func() { errc <- r.serve(context.Background(), server) }()
	go func() {
		client.Write([]byte("GET /v1/bandit/callback?token=tok-1 HTTP/1.1\r\nHost: api.example.test\r\nX-Pad: "))
		pad := make([]byte, 1024)
		for i := range pad {
			pad[i] = 'a'
		}
		for i := 0; i < 64; i++ {
			if _, err := client.Write(pad); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-errc:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("an oversized request was not rejected")
	}
	assert.Empty(t, rec.calls())
}

func TestSendCallback_DoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	t.Cleanup(target.Close)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(api.Close)

	cfg, err := newConfig(option.BanditProbeOutboundOptions{CallbackURL: api.URL + "/v1/bandit/callback"})
	require.NoError(t, err)
	client := newCallbackClient()
	client.Transport = api.Client().Transport
	r := &responder{cfg: cfg, httpClient: client, now: time.Now}

	err = r.sendCallback(context.Background(), url.Values{"token": {"tok-1"}}, "", result{verdict: verdictDelivered})
	assert.Error(t, err, "a 3xx must fail the callback")
	assert.False(t, redirected, "the callback must not follow a redirect")
}

func TestServe_RejectsUnexpectedRequests(t *testing.T) {
	tests := []struct {
		name   string
		target string
		status int
	}{
		{"wrong path", "http://api.example.test/v1/other?token=tok-1", http.StatusNotFound},
		{"missing token", "http://api.example.test/v1/bandit/callback?did=dev-1", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newCallbackRecorder(t)
			r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, scriptedState(sendState{}))
			server, client := net.Pipe()
			t.Cleanup(func() { client.Close(); server.Close() })

			resp, _, err := runProbe(t, r, server, client, tt.target)
			assert.Error(t, err)
			assert.Equal(t, tt.status, resp.StatusCode)
			assert.Empty(t, rec.calls())
		})
	}
}

func TestNewConfig(t *testing.T) {
	cfg, err := newConfig(option.BanditProbeOutboundOptions{CallbackURL: "https://api.example.test/v1/bandit/callback"})
	require.NoError(t, err)
	assert.Equal(t, defaultBodySize, cfg.bodySize)
	assert.Equal(t, defaultStallTimeout, cfg.stallTimeout)
	assert.Equal(t, defaultMaxWait, cfg.maxWait)
	assert.False(t, cfg.reportStalled)

	for _, bad := range []option.BanditProbeOutboundOptions{
		{},
		{CallbackURL: "api.example.test/v1/bandit/callback"},
		{CallbackURL: "ftp://api.example.test/x"},
		{CallbackURL: "http://api.example.test/v1/bandit/callback"},
		{CallbackURL: "https://api.example.test"},
		{CallbackURL: "https://api.example.test/"},
		{CallbackURL: "https://api.example.test/x", MaxWait: badoption.Duration(maxMaxWait + time.Second)},
		{CallbackURL: "https://api.example.test/x", BodySize: bodyPoolSize + 1},
	} {
		_, err := newConfig(bad)
		assert.Error(t, err, "%+v", bad)
	}
}

// debugLogger formats every message the way a box logging at debug does;
// the NOP logger the other tests use never formats its arguments.
func debugLogger() log.ContextLogger {
	factory := log.NewDefaultFactory(context.Background(), log.Formatter{}, io.Discard, "", nil, false)
	factory.SetLevel(log.LevelDebug)
	return factory.Logger()
}

func TestServe_DebugLoggingFormatsEveryVerdict(t *testing.T) {
	tests := []struct {
		name      string
		opts      option.BanditProbeOutboundOptions
		readState func(*net.TCPConn) (sendState, error)
		pipe      bool
		want      string
	}{
		{"delivered", option.BanditProbeOutboundOptions{}, scriptedState(
			sendState{acked: 4100, unacked: 0, retrans: 1, rtt: 42 * time.Millisecond},
		), false, "delivered"},
		{"stalled", option.BanditProbeOutboundOptions{ReportStalled: true}, scriptedState(
			sendState{acked: 100, unacked: 4000},
			sendState{acked: 1500, unacked: 2600, retrans: 7},
		), false, "stalled"},
		{"unknown", option.BanditProbeOutboundOptions{}, nil, true, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newCallbackRecorder(t)
			r := newTestResponder(t, rec, tt.opts, tt.readState)
			r.logger = debugLogger()
			var server, client net.Conn
			if tt.pipe {
				server, client = net.Pipe()
				t.Cleanup(func() { client.Close(); server.Close() })
			} else {
				server, client = tcpPair(t)
			}

			_, _, err := runProbe(t, r, server, client, probeTarget)
			require.NoError(t, err)
			calls := rec.calls()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.want, calls[0].Get("verdict"))
		})
	}
}

// panicConn panics on the first call serve makes, standing in for any bug in
// the probe path.
type panicConn struct{ net.Conn }

func (panicConn) SetReadDeadline(time.Time) error { panic("boom") }

func TestNewConnectionEx_RecoversFromPanic(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, nil)
	o := &Outbound{responder: r}
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	closed := make(chan error, 1)
	o.NewConnectionEx(context.Background(), panicConn{server}, adapter.InboundContext{}, func(err error) { closed <- err })
	select {
	case err := <-closed:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "panic answering probe")
	case <-time.After(5 * time.Second):
		t.Fatal("onClose was not called after the panic")
	}
}

// writePanicConn panics in Write, standing in for a buggy protocol wrapper. It
// exposes the wrapped conn as its upstream, as sing's wrappers do, so a TCP
// conn underneath is still found.
type writePanicConn struct{ net.Conn }

func (writePanicConn) Write([]byte) (int, error) { panic("boom") }
func (c writePanicConn) Upstream() any           { return c.Conn }

func TestServe_PanickingWriteIsAbortedNotReported(t *testing.T) {
	tests := []struct {
		name string
		pair func(t *testing.T) (net.Conn, net.Conn)
	}{
		{"non-TCP", func(t *testing.T) (net.Conn, net.Conn) {
			server, client := net.Pipe()
			t.Cleanup(func() { client.Close(); server.Close() })
			return server, client
		}},
		{"TCP", func(t *testing.T) (net.Conn, net.Conn) {
			server, client := tcpPair(t)
			return server, client
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newCallbackRecorder(t)
			r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true},
				scriptedState(sendState{acked: 0, unacked: 4096}))
			logs := &syncBuffer{}
			r.logger = errorLogger(logs)
			server, client := tt.pair(t)

			errc := make(chan error, 1)
			go func() { errc <- r.serve(context.Background(), writePanicConn{server}) }()
			req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
			require.NoError(t, err)
			require.NoError(t, req.Write(client))
			select {
			case err := <-errc:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not return after the write panicked")
			}
			assert.Empty(t, rec.calls(), "a panicking write is our bug, not a stall, so nothing is reported")
			assert.Contains(t, logs.String(), "panic writing probe response", "the recovered panic is logged at error level")
		})
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// errorLogger logs only errors, into w, as a box above debug level does.
func errorLogger(w io.Writer) log.ContextLogger {
	factory := log.NewDefaultFactory(context.Background(), log.Formatter{}, w, "", nil, false)
	factory.SetLevel(log.LevelError)
	return factory.Logger()
}

// latePanicConn blocks in Write until the responder aborts it by setting a
// past write deadline, then panics: a panic that lands after the poll loop
// last checked the write.
type latePanicConn struct {
	net.Conn
	release chan struct{}
	once    sync.Once
}

func (c *latePanicConn) SetWriteDeadline(t time.Time) error {
	if !t.After(time.Now()) {
		c.once.Do(func() { close(c.release) })
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *latePanicConn) Write([]byte) (int, error) {
	<-c.release
	panic("late boom")
}

func (c *latePanicConn) Upstream() any { return c.Conn }

func TestServe_WritePanicDuringStallIsAborted(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{ReportStalled: true},
		scriptedState(sendState{acked: 0, unacked: 4096}))
	server, client := tcpPair(t)
	conn := &latePanicConn{Conn: server, release: make(chan struct{})}

	errc := make(chan error, 1)
	go func() { errc <- r.serve(context.Background(), conn) }()
	req, err := http.NewRequest(http.MethodGet, probeTarget, nil)
	require.NoError(t, err)
	require.NoError(t, req.Write(client))
	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	assert.Empty(t, rec.calls(), "a write that panics while the stall is being called is aborted, not reported as stalled")
}

// ss2022MaxChunk is the largest payload the shadowsocks 2022 inbound frames as
// one chunk.
const ss2022MaxChunk = 64*1024 - 1

// chunkLimitedConn panics on a Write larger than limit, the way the
// shadowsocks 2022 inbound does on a chunk past its maximum.
type chunkLimitedConn struct {
	net.Conn
	limit int
}

func (c *chunkLimitedConn) Write(b []byte) (int, error) {
	if len(b) > c.limit {
		panic(fmt.Sprintf("buffer overflow: write of %d past chunk limit %d", len(b), c.limit))
	}
	return c.Conn.Write(b)
}

func TestServe_ResponseFitsChunkLimitedInbound(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{BodySize: defaultBodySize}, nil)
	server, client := tcpPair(t)
	conn := &chunkLimitedConn{Conn: server, limit: ss2022MaxChunk}

	resp, body, err := runProbe(t, r, conn, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, defaultBodySize)
	calls := rec.calls()
	require.Len(t, calls, 1, "a probe through a chunk-limited inbound must be reported")
	assert.Equal(t, "unknown", calls[0].Get("verdict"))
}

// shortWriteConn accepts at most limit bytes per Write and reports a short
// count with no error.
type shortWriteConn struct {
	net.Conn
	limit int
}

func (c *shortWriteConn) Write(b []byte) (int, error) {
	return c.Conn.Write(b[:min(len(b), c.limit)])
}

func TestServe_ShortWritesStillSendTheWholeResponse(t *testing.T) {
	rec := newCallbackRecorder(t)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{BodySize: defaultBodySize}, nil)
	server, client := tcpPair(t)

	resp, body, err := runProbe(t, r, &shortWriteConn{Conn: server, limit: 1000}, client, probeTarget)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, body, defaultBodySize, "a short write must not drop the rest of a chunk")
}
