package banditprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

type batchRecorder struct {
	mu      sync.Mutex
	status  int
	auth    []string
	batches [][]batchCallback
	got     chan struct{}
}

func newBatchRecorder(t *testing.T, status int) (*batchRecorder, *httptest.Server) {
	rec := &batchRecorder{status: status, got: make(chan struct{}, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Callbacks []batchCallback `json:"callbacks"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.batches = append(rec.batches, body.Callbacks)
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
		rec.got <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

type singleRecorder struct {
	mu     sync.Mutex
	tokens []string
}

func (s *singleRecorder) send(_ context.Context, q url.Values, _ string, _ result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, q.Get("token"))
	return nil
}

func testBatcher(t *testing.T, srvURL string, interval time.Duration, maxSize int, single *singleRecorder) *batcher {
	u, err := url.Parse(srvURL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	return newBatcher(batchConfig{url: u, token: "fleet-token", interval: interval, maxSize: maxSize},
		http.DefaultClient, log.NewNOPFactory().Logger(), time.Now, single.send)
}

func probeQuery(token string) url.Values {
	return url.Values{"token": {token}, "did": {"dev-1"}, "tp": {"tp-1"}, "dl": {"tryjeo"}, "cd": {"40"}}
}

// Closing the batcher sends what is waiting as one batch with the fleet's
// token and every field the API takes.
func TestBatcherSendsWaitingCallbacksOnClose(t *testing.T) {
	rec, srv := newBatchRecorder(t, http.StatusOK)
	single := &singleRecorder{}
	b := testBatcher(t, srv.URL, time.Hour, 100, single)
	b.add(probeQuery("tok-1"), "", result{verdict: verdictDelivered, drain: 120 * time.Millisecond, acked: 65632,
		state: sendState{retrans: 2, rtt: 85 * time.Millisecond}})
	b.add(probeQuery("tok-2"), "", result{verdict: verdictUnknown, drain: 10 * time.Millisecond, acked: 99})
	b.close()

	require.Len(t, rec.batches, 1)
	assert.Equal(t, []string{"Bearer fleet-token"}, rec.auth)
	got := rec.batches[0]
	require.Len(t, got, 2)
	assert.Equal(t, batchCallback{Token: "tok-1", DeviceID: "dev-1", Traceparent: "tp-1", Deadline: "tryjeo",
		ClientQueueDelayMs: 40, Verdict: "delivered", DrainMs: 120, AckedBytes: 65632, Retransmits: 2, RttMs: 85,
		QueuedMs: got[0].QueuedMs}, got[0])
	assert.Zero(t, got[1].AckedBytes, "an unknown verdict carries no TCP observations")
	assert.Empty(t, single.tokens)
}

// A batch goes out as soon as maxSize callbacks are waiting.
func TestBatcherSendsAFullBatchEarly(t *testing.T) {
	rec, srv := newBatchRecorder(t, http.StatusOK)
	b := testBatcher(t, srv.URL, time.Hour, 2, &singleRecorder{})
	defer b.close()
	b.add(probeQuery("tok-1"), "", result{verdict: verdictDelivered})
	b.add(probeQuery("tok-2"), "", result{verdict: verdictDelivered})
	select {
	case <-rec.got:
	case <-time.After(5 * time.Second):
		t.Fatal("a full batch was not sent")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.batches, 1)
	assert.Len(t, rec.batches[0], 2)
}

// A batch the API refuses goes out as single callbacks.
func TestBatcherFallsBackToSingleCallbacks(t *testing.T) {
	_, srv := newBatchRecorder(t, http.StatusServiceUnavailable)
	single := &singleRecorder{}
	b := testBatcher(t, srv.URL, time.Hour, 100, single)
	b.add(probeQuery("tok-1"), "", result{verdict: verdictDelivered})
	b.add(probeQuery("tok-2"), "", result{verdict: verdictStalled})
	b.close()
	assert.ElementsMatch(t, []string{"tok-1", "tok-2"}, single.tokens)
}

// A callback added after the batcher closed goes out on its own.
func TestBatcherSendsLateCallbacksSingly(t *testing.T) {
	rec, srv := newBatchRecorder(t, http.StatusOK)
	single := &singleRecorder{}
	b := testBatcher(t, srv.URL, time.Hour, 100, single)
	b.close()
	b.add(probeQuery("late"), "", result{verdict: verdictDelivered})
	assert.Equal(t, []string{"late"}, single.tokens, "sent before add returns")
	assert.Empty(t, rec.batches)
}

// Single callbacks for a refused batch don't hold up the next batch.
func TestBatcherKeepsBatchingWhileFallingBack(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	got := make(chan int, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		got <- n
	}))
	t.Cleanup(srv.Close)
	release := make(chan struct{})
	blocked := &singleRecorder{}
	u, err := url.Parse(srv.URL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	b := newBatcher(batchConfig{url: u, token: "fleet-token", interval: time.Hour, maxSize: 1},
		http.DefaultClient, log.NewNOPFactory().Logger(), time.Now,
		func(ctx context.Context, q url.Values, tp string, res result) error {
			<-release
			return blocked.send(ctx, q, tp, res)
		})
	defer func() {
		close(release)
		b.close()
	}()

	b.add(probeQuery("refused"), "", result{verdict: verdictDelivered})
	require.Equal(t, 1, <-got)
	b.add(probeQuery("next"), "", result{verdict: verdictDelivered})
	select {
	case n := <-got:
		assert.Equal(t, 2, n, "the next batch went out while the fallback waited")
	case <-time.After(5 * time.Second):
		t.Fatal("the next batch waited for the fallback")
	}
}

// A batched callback carries callback_url's own parameters over the
// client's, as a single callback does.
func TestServe_BatchAppliesCallbackURLParams(t *testing.T) {
	rec := newCallbackRecorder(t)
	batches, srv := newBatchRecorder(t, http.StatusOK)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{CallbackURL: rec.srv.URL + "/v1/bandit/callback?cd=0"}, scriptedState(
		sendState{acked: 100, unacked: 4000},
		sendState{acked: 4100, unacked: 0},
	))
	u, err := url.Parse(srv.URL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	r.batch = newBatcher(batchConfig{url: u, token: "fleet-token", interval: time.Hour, maxSize: 100},
		srv.Client(), r.logger, r.now, r.sendCallback)
	server, client := tcpPair(t)
	_, _, err = runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	r.batch.close()
	require.Len(t, batches.batches, 1)
	assert.Zero(t, batches.batches[0][0].ClientQueueDelayMs, "callback_url's cd=0 overrides the client's cd=40")
}

func TestToBatchCallbackQueueTime(t *testing.T) {
	queued := time.Unix(1_800_000_000, 0)
	cb := toBatchCallback(queuedCallback{query: url.Values{"token": {"t"}}, traceparent: "hdr-tp",
		res: result{verdict: verdictDelivered}, queued: queued}, queued.Add(750*time.Millisecond))
	assert.Equal(t, int64(750), cb.QueuedMs)
	assert.Equal(t, "hdr-tp", cb.Traceparent, "the header's traceparent when the URL carries none")
}

func TestNewBatchConfig(t *testing.T) {
	cfg, err := newBatchConfig("", "", 0, 0)
	require.NoError(t, err)
	assert.Nil(t, cfg.url, "no batch URL, no batching")

	cfg, err = newBatchConfig("https://api.example.test/v1/bandit/callbacks", "tok", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, defaultBatchInterval, cfg.interval)
	assert.Equal(t, defaultBatchMaxSize, cfg.maxSize)

	for name, tc := range map[string]struct {
		url, token string
		interval   time.Duration
		maxSize    int
	}{
		"http":          {url: "http://api.example.test/v1/bandit/callbacks", token: "tok"},
		"no token":      {url: "https://api.example.test/v1/bandit/callbacks"},
		"long interval": {url: "https://api.example.test/x", token: "tok", interval: 5 * time.Second},
		"big batches":   {url: "https://api.example.test/x", token: "tok", maxSize: 1001},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newBatchConfig(tc.url, tc.token, tc.interval, tc.maxSize)
			assert.Error(t, err)
		})
	}
}

// With batching on, a served probe's callback joins a batch instead of going
// out on its own.
func TestServe_BatchesTheCallback(t *testing.T) {
	rec := newCallbackRecorder(t)
	batches, srv := newBatchRecorder(t, http.StatusOK)
	r := newTestResponder(t, rec, option.BanditProbeOutboundOptions{}, scriptedState(
		sendState{acked: 100, unacked: 4000},
		sendState{acked: 4100, unacked: 0, retrans: 1, rtt: 42 * time.Millisecond},
	))
	u, err := url.Parse(srv.URL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	r.batch = newBatcher(batchConfig{url: u, token: "fleet-token", interval: time.Hour, maxSize: 100},
		srv.Client(), r.logger, r.now, r.sendCallback)
	server, client := tcpPair(t)

	_, _, err = runProbe(t, r, server, client, probeTarget)
	require.NoError(t, err)
	r.batch.close()

	assert.Empty(t, rec.calls(), "no single callback")
	require.Len(t, batches.batches, 1)
	require.Len(t, batches.batches[0], 1)
	cb := batches.batches[0][0]
	assert.Equal(t, "tok-1", cb.Token)
	assert.Equal(t, "dev-1", cb.DeviceID)
	assert.Equal(t, "tp-1", cb.Traceparent)
	assert.Equal(t, int64(40), cb.ClientQueueDelayMs)
	assert.Equal(t, "delivered", cb.Verdict)
	assert.Equal(t, uint64(4100), cb.AckedBytes)
}

// close returns after its drain timeout while slow single callbacks are
// still queued; the workers then send every one and exit.
func TestBatcherDrainsFallbacksAfterClose(t *testing.T) {
	_, srv := newBatchRecorder(t, http.StatusServiceUnavailable)
	release := make(chan struct{})
	single := &singleRecorder{}
	u, err := url.Parse(srv.URL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	b := newBatcher(batchConfig{url: u, token: "fleet-token", interval: time.Hour, maxSize: 100},
		http.DefaultClient, log.NewNOPFactory().Logger(), time.Now,
		func(ctx context.Context, q url.Values, tp string, res result) error {
			<-release
			return single.send(ctx, q, tp, res)
		})
	b.closeTimeout = 100 * time.Millisecond
	n := fallbackWorkers + 5
	for i := range n {
		b.add(probeQuery(fmt.Sprintf("tok-%d", i)), "", result{verdict: verdictDelivered})
	}
	b.close()
	single.mu.Lock()
	assert.Empty(t, single.tokens, "close returned while every worker was blocked")
	single.mu.Unlock()
	close(release)
	b.fallbacks.Wait()
	single.mu.Lock()
	defer single.mu.Unlock()
	assert.Len(t, single.tokens, n, "every queued single callback is sent")
}

// close returns within its timeout when the batch endpoint hangs with
// several batches waiting, rather than waiting out each batch's request.
func TestBatcherCloseBoundsTheFinalFlush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices the client hang up only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	single := &singleRecorder{}
	b := testBatcher(t, srv.URL, time.Hour, 1, single)
	b.closeTimeout = 200 * time.Millisecond
	for i := range 5 {
		b.add(probeQuery(fmt.Sprintf("tok-%d", i)), "", result{verdict: verdictDelivered})
	}
	start := time.Now()
	b.close()
	assert.Less(t, time.Since(start), 2*time.Second, "close waits out one shared timeout")
	b.fallbacks.Wait()
	single.mu.Lock()
	defer single.mu.Unlock()
	assert.Empty(t, single.tokens, "batches unsent at the deadline are dropped, not sent singly")
}

// A refused batch's single callback carries the time it waited before going
// out in its drain time, which the single endpoint deducts from its latency.
func TestBatcherFallbackCreditsQueueTimeAsDrain(t *testing.T) {
	_, srv := newBatchRecorder(t, http.StatusServiceUnavailable)
	u, err := url.Parse(srv.URL + "/v1/bandit/callbacks")
	require.NoError(t, err)
	var clock atomic.Int64
	start := time.Now()
	now := func() time.Time { return start.Add(time.Duration(clock.Load())) }
	drains := make(chan time.Duration, 1)
	b := newBatcher(batchConfig{url: u, token: "fleet-token", interval: time.Hour, maxSize: 100},
		http.DefaultClient, log.NewNOPFactory().Logger(), now,
		func(_ context.Context, _ url.Values, _ string, res result) error {
			drains <- res.drain
			return nil
		})
	b.add(probeQuery("tok-1"), "", result{verdict: verdictDelivered, drain: 50 * time.Millisecond})
	clock.Store(int64(3 * time.Second))
	b.close()
	assert.Equal(t, 3050*time.Millisecond, <-drains)
}
