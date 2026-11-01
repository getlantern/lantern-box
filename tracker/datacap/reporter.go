package datacap

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// closeFlushBudget bounds the final flush on Close, so an unreachable sidecar
// cannot hold up a shutdown or reload.
const closeFlushBudget = 5 * time.Second

// reportWorkers bounds the concurrent sidecar requests of one flush. The
// sidecar runs on the same host, so a few workers drain thousands of devices
// well within a report interval.
const reportWorkers = 8

// Traffic counter slots, in TrafficUsage order: bytes, then bidirectional
// connections, for the lantern, probe, other and unknown categories.
const (
	trafficLantern = iota
	trafficProbe
	trafficOther
	trafficUnknown
	trafficCategoryCount
)

func trafficSlot(category string) int {
	switch category {
	case "lantern":
		return trafficLantern
	case "probe":
		return trafficProbe
	case "other":
		return trafficOther
	default:
		return trafficUnknown
	}
}

// Reporter aggregates usage per device and reports each device's delta to the
// sidecar once per interval. A device often has dozens of open connections, and
// a busy proxy tens of thousands, so reporting per connection costs a goroutine,
// a ticker and an HTTP request per connection per interval.
type Reporter struct {
	client   *Client
	logger   log.ContextLogger
	interval time.Duration

	mu      sync.Mutex
	devices map[string]*deviceUsage

	flushMu sync.Mutex // serializes flushes so restored deltas keep their order
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// deviceUsage holds a device's usage not yet accepted by the sidecar.
type deviceUsage struct {
	deviceID  string
	throttler *Throttler

	info  atomic.Pointer[clientcontext.ClientInfo] // latest country and platform
	conns atomic.Int64                             // open connections; guarded by Reporter.mu for eviction

	// Each byte is counted in exactly one counter, and a report's total is
	// the sum of what it takes from them, so the categories of a report can
	// never exceed its total however writes interleave with a flush.
	uncategorized  atomic.Int64
	trafficBytes   [trafficCategoryCount]atomic.Int64
	trafficConns   [trafficCategoryCount]atomic.Int64
	hasTrafficInfo atomic.Bool
}

func (d *deviceUsage) addBytes(n int64, category string) {
	if category == "" {
		d.uncategorized.Add(n)
		return
	}
	d.hasTrafficInfo.Store(true)
	d.trafficBytes[trafficSlot(category)].Add(n)
}

// pendingBytes returns the device's bytes not yet taken for a report.
func (d *deviceUsage) pendingBytes() int64 {
	n := d.uncategorized.Load()
	for i := range trafficCategoryCount {
		n += d.trafficBytes[i].Load()
	}
	return n
}

func (d *deviceUsage) addConnection(category string) {
	d.hasTrafficInfo.Store(true)
	d.trafficConns[trafficSlot(category)].Add(1)
}

// usageDelta is one device's usage taken for a report.
type usageDelta struct {
	bytes         int64 // total, including uncategorized
	uncategorized int64
	trafficBytes  [trafficCategoryCount]int64
	trafficConns  [trafficCategoryCount]int64
	hasTraffic    bool
}

func (d *deviceUsage) take() usageDelta {
	u := usageDelta{uncategorized: d.uncategorized.Swap(0), hasTraffic: d.hasTrafficInfo.Load()}
	u.bytes = u.uncategorized
	for i := range trafficCategoryCount {
		u.trafficBytes[i] = d.trafficBytes[i].Swap(0)
		u.bytes += u.trafficBytes[i]
		// The sidecar rejects a report with more connections than bytes in a
		// category. A connection can turn bidirectional after its bytes were
		// taken, so hold such connections for a report that carries bytes.
		conns := d.trafficConns[i].Swap(0)
		if conns > u.trafficBytes[i] {
			d.trafficConns[i].Add(conns - u.trafficBytes[i])
			conns = u.trafficBytes[i]
		}
		u.trafficConns[i] = conns
	}
	return u
}

func (d *deviceUsage) restore(u usageDelta) {
	d.uncategorized.Add(u.uncategorized)
	for i := range trafficCategoryCount {
		d.trafficBytes[i].Add(u.trafficBytes[i])
		d.trafficConns[i].Add(u.trafficConns[i])
	}
	if u.hasTraffic {
		d.hasTrafficInfo.Store(true)
	}
}

func (u usageDelta) trafficUsage() *TrafficUsage {
	if !u.hasTraffic {
		return nil
	}
	return &TrafficUsage{
		LanternBytes:       u.trafficBytes[trafficLantern],
		ProbeBytes:         u.trafficBytes[trafficProbe],
		OtherBytes:         u.trafficBytes[trafficOther],
		UnknownBytes:       u.trafficBytes[trafficUnknown],
		LanternConnections: u.trafficConns[trafficLantern],
		ProbeConnections:   u.trafficConns[trafficProbe],
		OtherConnections:   u.trafficConns[trafficOther],
		UnknownConnections: u.trafficConns[trafficUnknown],
	}
}

// NewReporter starts a reporter that flushes every interval until Close.
func NewReporter(client *Client, logger log.ContextLogger, interval time.Duration) *Reporter {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	r := &Reporter{
		client:   client,
		logger:   logger,
		interval: interval,
		devices:  make(map[string]*deviceUsage),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *Reporter) run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.flush(context.Background())
		case <-r.stop:
			ctx, cancel := context.WithTimeout(context.Background(), closeFlushBudget)
			r.flush(ctx)
			cancel()
			return
		}
	}
}

// Close stops the flush loop after a final flush, and returns once that
// flush is done.
func (r *Reporter) Close() {
	r.once.Do(func() { close(r.stop) })
	<-r.done
}

// acquire returns the usage entry for info's device and counts a connection
// on it. Every acquire must be paired with a release.
func (r *Reporter) acquire(info clientcontext.ClientInfo, throttler *Throttler) *deviceUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[info.DeviceID]
	if !ok {
		d = &deviceUsage{deviceID: info.DeviceID, throttler: throttler}
		r.devices[info.DeviceID] = d
	}
	d.info.Store(&info)
	d.conns.Add(1)
	return d
}

func (r *Reporter) release(d *deviceUsage) {
	d.conns.Add(-1)
}

// flush reports every device's pending usage and drops devices with no open
// connections. Their final usage is part of this flush.
func (r *Reporter) flush(ctx context.Context) {
	if r.client == nil {
		return
	}
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	type pending struct {
		d *deviceUsage
		u usageDelta
	}
	var reports []pending
	r.mu.Lock()
	for id, d := range r.devices {
		if d.conns.Load() == 0 {
			delete(r.devices, id)
		}
		if u := d.take(); u.bytes > 0 {
			reports = append(reports, pending{d, u})
		}
	}
	r.mu.Unlock()

	if len(reports) == 0 {
		return
	}
	work := make(chan pending)
	var wg sync.WaitGroup
	for range min(reportWorkers, len(reports)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				r.report(ctx, p.d, p.u)
			}
		}()
	}
	for _, p := range reports {
		work <- p
	}
	close(work)
	wg.Wait()
}

func (r *Reporter) report(ctx context.Context, d *deviceUsage, u usageDelta) {
	info := d.info.Load()
	report := &DataCapReport{
		DeviceID:     d.deviceID,
		CountryCode:  info.CountryCode,
		Platform:     info.Platform,
		BytesUsed:    u.bytes,
		TrafficUsage: u.trafficUsage(),
	}
	timeout := r.client.httpClient.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status, err := r.client.ReportDataCapConsumption(ctx, report)
	if err != nil {
		r.restore(d, u)
		r.logger.Debug("failed to report datacap consumption (will retry): ", err)
		return
	}
	r.logger.Debug("reported datacap delta: ", u.bytes, " bytes for device ", d.deviceID)
	if status != nil {
		updateThrottleState(r.logger, d.throttler, status)
	}
}

// restore puts back usage the sidecar did not accept so the next flush
// reports it again, re-adding the device if the flush dropped it.
func (r *Reporter) restore(d *deviceUsage, u usageDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.devices[d.deviceID]; ok {
		d = cur
	} else {
		r.devices[d.deviceID] = d
	}
	d.restore(u)
}

// updateThrottleState applies a sidecar status to a device's throttler.
func updateThrottleState(logger log.ContextLogger, throttler *Throttler, status *DataCapStatus) {
	if throttler == nil {
		logger.Debug("throttler not initialized, skipping update")
		return
	}
	if status.Throttle {
		// We want to throttle Download (Write) but keep Upload (Read) fast enough for requests.
		throttler.UpdateRates(defaultUploadSpeedBytesPerSec, lowTierSpeedBytesPerSec)
		logger.Debug("data cap exhausted, throttling at ", lowTierSpeedBytesPerSec, " bytes/s")
	} else {
		throttler.Disable()
		logger.Debug("throttling disabled - full speed")
	}
}
