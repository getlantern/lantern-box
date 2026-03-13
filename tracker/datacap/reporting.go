package datacap

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

const (
	defaultReportInterval = 10 * time.Second
	reportTimeout         = 10 * time.Second
	statusTimeout         = 5 * time.Second
	shutdownGracePeriod   = 10 * time.Second
	finalUploadTimeout    = 5 * time.Second
	defaultExpiryDuration = 24 * time.Hour

	// Default upload speed (not throttled to allow user uploads even when capped)
	defaultUploadSpeedBytesPerSec = 640 * 1024 // 5 Mb/s (640 KB/s)

	lowTierSpeedBytesPerSec = 16 * 1024 // 128 Kb/s (16 KB/s)
)

// reportingCore contains shared reporting, throttling, and lifecycle logic
// used by both Conn and PacketConn.
type reportingCore struct {
	ctx    context.Context
	cancel context.CancelFunc
	sink   ReportSink
	logger log.ContextLogger

	clientInfo clientcontext.ClientInfo

	bytesSent     atomic.Int64
	bytesReceived atomic.Int64

	reportTicker *time.Ticker
	reportMutex  sync.Mutex
	closed       atomic.Bool
	wg           sync.WaitGroup

	throttler *Throttler
}

type reportingConfig struct {
	Sink           ReportSink
	Logger         log.ContextLogger
	ClientInfo     clientcontext.ClientInfo
	ReportInterval time.Duration
	Throttler      *Throttler
}

func newReportingCore(cfg reportingConfig) reportingCore {
	if cfg.ReportInterval <= 0 {
		cfg.ReportInterval = defaultReportInterval
	}
	throttler := cfg.Throttler
	if throttler == nil {
		throttler = NewThrottler(0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return reportingCore{
		ctx:          ctx,
		cancel:       cancel,
		sink:         cfg.Sink,
		logger:       cfg.Logger,
		clientInfo:   cfg.ClientInfo,
		reportTicker: time.NewTicker(cfg.ReportInterval),
		throttler:    throttler,
	}
}

func (r *reportingCore) startPeriodicReport() {
	r.wg.Add(1)
	go r.periodicReport()
}

func (r *reportingCore) periodicReport() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.reportTicker.C:
			r.sendReport()
		}
	}
}

// closeReporting stops the ticker, cancels the context, waits for goroutines,
// and sends a final report. The caller is responsible for closing the underlying connection.
func (r *reportingCore) closeReporting() bool {
	if r.closed.Swap(true) {
		return false
	}
	r.reportTicker.Stop()
	r.cancel()
	r.wg.Wait()
	r.sendReport()
	return true
}

func (r *reportingCore) sendReport() {
	r.reportMutex.Lock()
	defer r.reportMutex.Unlock()

	if r.sink == nil {
		return
	}
	sent := r.bytesSent.Swap(0)
	received := r.bytesReceived.Swap(0)
	delta := sent + received

	if delta == 0 {
		return
	}

	report := &DataCapReport{
		DeviceID:    r.clientInfo.DeviceID,
		CountryCode: r.clientInfo.CountryCode,
		Platform:    r.clientInfo.Platform,
		BytesUsed:   delta,
	}

	reportCtx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()

	status, err := r.sink.Report(reportCtx, report)
	if err != nil {
		r.bytesSent.Add(sent)
		r.bytesReceived.Add(received)
		r.logger.Debug("failed to report datacap consumption (will retry): ", err)
	} else {
		r.logger.Debug("reported datacap delta: ", delta, " bytes (sent: ", sent, ", received: ", received, ") for device ", r.clientInfo.DeviceID)
		if status != nil {
			r.updateThrottleState(status)
		}
	}
}

func (r *reportingCore) updateThrottleState(status *DataCapStatus) {
	if status.Throttle {
		r.throttler.UpdateRates(defaultUploadSpeedBytesPerSec, lowTierSpeedBytesPerSec)
		r.logger.Debug("data cap exhausted, throttling at ", lowTierSpeedBytesPerSec, " bytes/s")
	} else {
		r.throttler.Disable()
		r.logger.Debug("throttling disabled - full speed")
	}
}

func (r *reportingCore) getStatus() (*DataCapStatus, error) {
	if r.sink == nil {
		return nil, nil
	}
	statusCtx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	return r.sink.Report(statusCtx, &DataCapReport{
		DeviceID:    r.clientInfo.DeviceID,
		CountryCode: r.clientInfo.CountryCode,
		Platform:    r.clientInfo.Platform,
		BytesUsed:   0,
	})
}

func (r *reportingCore) getBytesConsumed() int64 {
	return r.bytesSent.Load() + r.bytesReceived.Load()
}
