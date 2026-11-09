package banditprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/sagernet/sing/common/logger"
)

const (
	defaultBatchInterval = time.Second
	// maxBatchInterval keeps every callback's queue time under the 5s the
	// API credits.
	maxBatchInterval    = 4 * time.Second
	defaultBatchMaxSize = 200
	maxBatchMaxSize     = 1000
	// maxBatchBacklog bounds the callbacks held while batches fail, in
	// batches of batchMaxSize.
	maxBatchBacklog = 10
)

// batchConfig configures sending callbacks in batches; a zero url sends
// them one at a time.
type batchConfig struct {
	url      *url.URL
	token    string
	interval time.Duration
	maxSize  int
}

// queuedCallback is a callback waiting for its batch.
type queuedCallback struct {
	query       url.Values
	traceparent string
	res         result
	queued      time.Time
}

// batchCallback is one callback of a batch, as the API's
// ReportBanditCallbacksRequest takes it.
type batchCallback struct {
	Token              string `json:"token"`
	DeviceID           string `json:"device_id,omitempty"`
	Traceparent        string `json:"traceparent,omitempty"`
	Deadline           string `json:"deadline,omitempty"`
	ClientQueueDelayMs int64  `json:"client_queue_delay_ms,omitempty"`
	Verdict            string `json:"verdict"`
	DrainMs            int64  `json:"drain_ms"`
	AckedBytes         uint64 `json:"acked_bytes,omitempty"`
	Retransmits        uint64 `json:"retransmits,omitempty"`
	RttMs              int64  `json:"rtt_ms,omitempty"`
	QueuedMs           int64  `json:"queued_ms"`
}

// batcher holds callbacks and sends them to the batch endpoint every
// interval, or as soon as maxSize are waiting. A batch the endpoint refuses,
// or that fails to arrive, goes out as single callbacks.
type batcher struct {
	cfg    batchConfig
	client *http.Client
	logger logger.ContextLogger
	now    func() time.Time
	single func(ctx context.Context, query url.Values, traceparent string, res result) error

	mu      sync.Mutex
	pending []queuedCallback
	dropped int

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newBatcher(cfg batchConfig, client *http.Client, lg logger.ContextLogger, now func() time.Time,
	single func(context.Context, url.Values, string, result) error,
) *batcher {
	b := &batcher{cfg: cfg, client: client, logger: lg, now: now, single: single,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go b.run()
	return b
}

// add queues a callback for the next batch.
func (b *batcher) add(query url.Values, traceparent string, res result) {
	b.mu.Lock()
	if len(b.pending) >= b.cfg.maxSize*maxBatchBacklog {
		b.dropped++
		b.mu.Unlock()
		return
	}
	b.pending = append(b.pending, queuedCallback{query: query, traceparent: traceparent, res: res, queued: b.now()})
	full := len(b.pending) >= b.cfg.maxSize
	b.mu.Unlock()
	if full {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

func (b *batcher) run() {
	defer close(b.done)
	ticker := time.NewTicker(b.cfg.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-b.kick:
		case <-b.stop:
			b.flush()
			return
		}
		b.flush()
	}
}

// close sends what is waiting and stops the batcher.
func (b *batcher) close() {
	close(b.stop)
	<-b.done
}

// flush sends every waiting callback, maxSize to a batch.
func (b *batcher) flush() {
	b.mu.Lock()
	pending, dropped := b.pending, b.dropped
	b.pending, b.dropped = nil, 0
	b.mu.Unlock()
	ctx := context.Background()
	if dropped > 0 {
		b.logger.WarnContext(ctx, "bandit probe callback batches backed up; dropped ", dropped, " callbacks")
	}
	for len(pending) > 0 {
		n := min(len(pending), b.cfg.maxSize)
		batch := pending[:n]
		pending = pending[n:]
		if err := b.send(ctx, batch); err != nil {
			b.logger.WarnContext(ctx, "bandit probe callback batch failed, sending its ", len(batch), " callbacks singly: ", err)
			for _, cb := range batch {
				if err := b.single(ctx, cb.query, cb.traceparent, cb.res); err != nil {
					b.logger.WarnContext(ctx, "bandit probe callback failed: ", err)
				}
			}
		}
	}
}

// send posts one batch.
func (b *batcher) send(ctx context.Context, batch []queuedCallback) error {
	sent := b.now()
	cbs := make([]batchCallback, len(batch))
	for i, cb := range batch {
		cbs[i] = toBatchCallback(cb, sent)
	}
	body, err := json.Marshal(struct {
		Callbacks []batchCallback `json:"callbacks"`
	}{cbs})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.url.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.cfg.token)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("batch returned %s", resp.Status)
	}
	return nil
}

// toBatchCallback is cb as the batch endpoint takes it, with the time it
// waited for its batch as of sent. Its fields are those sendCallback sends.
func toBatchCallback(cb queuedCallback, sent time.Time) batchCallback {
	q := cb.query
	tp := q.Get("tp")
	if tp == "" {
		tp = cb.traceparent
	}
	cd, _ := strconv.ParseInt(q.Get("cd"), 10, 64)
	out := batchCallback{
		Token:              q.Get("token"),
		DeviceID:           q.Get("did"),
		Traceparent:        tp,
		Deadline:           q.Get("dl"),
		ClientQueueDelayMs: max(cd, 0),
		Verdict:            string(cb.res.verdict),
		DrainMs:            cb.res.drain.Milliseconds(),
		QueuedMs:           max(sent.Sub(cb.queued).Milliseconds(), 0),
	}
	if cb.res.verdict != verdictUnknown {
		out.AckedBytes = cb.res.acked
		out.Retransmits = uint64(cb.res.state.retrans)
		out.RttMs = cb.res.state.rtt.Milliseconds()
	}
	return out
}

// newBatchConfig validates the batch options; an empty url turns batching
// off.
func newBatchConfig(rawURL, token string, interval time.Duration, maxSize int) (batchConfig, error) {
	if rawURL == "" {
		return batchConfig{}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return batchConfig{}, fmt.Errorf("banditprobe: batch_url must be an https URL, got %q", rawURL)
	}
	if token == "" {
		return batchConfig{}, errors.New("banditprobe: batch_url needs batch_token")
	}
	if interval <= 0 {
		interval = defaultBatchInterval
	}
	if interval > maxBatchInterval {
		return batchConfig{}, fmt.Errorf("banditprobe: batch_interval %s exceeds %s", interval, maxBatchInterval)
	}
	if maxSize <= 0 {
		maxSize = defaultBatchMaxSize
	}
	if maxSize > maxBatchMaxSize {
		return batchConfig{}, fmt.Errorf("banditprobe: batch_max_size %d exceeds %d", maxSize, maxBatchMaxSize)
	}
	return batchConfig{url: u, token: token, interval: interval, maxSize: maxSize}, nil
}
