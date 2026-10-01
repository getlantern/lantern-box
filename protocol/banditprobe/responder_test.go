package banditprobe

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

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
		{CallbackURL: "https://api.example.test/x", MaxWait: badoption.Duration(maxMaxWait + time.Second)},
		{CallbackURL: "https://api.example.test/x", BodySize: bodyPoolSize + 1},
	} {
		_, err := newConfig(bad)
		assert.Error(t, err, "%+v", bad)
	}
}
