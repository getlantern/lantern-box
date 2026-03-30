package datacap

import (
	"context"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

var _ (adapter.ConnectionTracker) = (*DatacapTracker)(nil)

type DatacapTracker struct {
	sink             ReportSink
	store            *DeviceUsageStore // nil in sidecar mode
	logger           log.ContextLogger
	reportInterval   time.Duration
	throttleRegistry *ThrottleRegistry
}

type Options struct {
	URL            string `json:"url,omitempty"`
	ReportInterval string `json:"report_interval,omitempty"`
	HTTPTimeout    string `json:"http_timeout,omitempty"`
}

// NewDatacapTracker creates a tracker that reports to the sidecar via HTTP (legacy mode).
func NewDatacapTracker(options Options, logger log.ContextLogger) (*DatacapTracker, error) {
	if options.URL == "" {
		return nil, E.New("datacap url not defined")
	}
	reportInterval := 10 * time.Second
	if options.ReportInterval != "" {
		interval, err := time.ParseDuration(options.ReportInterval)
		if err != nil {
			return nil, E.New("invalid report_interval: ", err)
		}
		reportInterval = interval
	}

	httpTimeout := 10 * time.Second
	if options.HTTPTimeout != "" {
		timeout, err := time.ParseDuration(options.HTTPTimeout)
		if err != nil {
			return nil, E.New("invalid http_timeout: ", err)
		}
		httpTimeout = timeout
	}
	return &DatacapTracker{
		sink:             NewClient(options.URL, httpTimeout),
		reportInterval:   reportInterval,
		throttleRegistry: NewThrottleRegistry(),
		logger:           logger,
	}, nil
}

// NewDatacapTrackerWithStore creates a tracker that uses an in-process DeviceUsageStore (direct mode).
func NewDatacapTrackerWithStore(store *DeviceUsageStore, reportInterval time.Duration, logger log.ContextLogger) *DatacapTracker {
	if reportInterval <= 0 {
		reportInterval = 10 * time.Second
	}
	return &DatacapTracker{
		sink:             store,
		store:            store,
		reportInterval:   reportInterval,
		throttleRegistry: NewThrottleRegistry(),
		logger:           logger,
	}
}

// Start launches background goroutines (only meaningful in direct mode).
func (t *DatacapTracker) Start(ctx context.Context) {
	if t.store != nil {
		t.store.Start(ctx)
	}
}

// Stop performs graceful shutdown (only meaningful in direct mode).
func (t *DatacapTracker) Stop(ctx context.Context) {
	if t.store != nil {
		t.store.Stop(ctx)
	}
}

func (t *DatacapTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if !ok {
		t.logger.Debug("skipping datacap: no client info in context")
		return conn
	}
	if info.IsPro {
		t.logger.Debug("skipping datacap: client is pro ", info.DeviceID)
		return conn
	}
	return NewConn(ConnConfig{
		Conn:           conn,
		Sink:           t.sink,
		Logger:         t.logger,
		ClientInfo:     info,
		ReportInterval: t.reportInterval,
		Throttler:      t.throttleRegistry.GetOrCreate(info.DeviceID),
	})
}

func (t *DatacapTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if !ok {
		t.logger.Debug("skipping datacap: no client info in context")
		return conn
	}
	if info.IsPro {
		t.logger.Debug("skipping datacap: client is pro ", info.DeviceID)
		return conn
	}
	return NewPacketConn(PacketConnConfig{
		Conn:           conn,
		Sink:           t.sink,
		Logger:         t.logger,
		ClientInfo:     info,
		ReportInterval: t.reportInterval,
		Throttler:      t.throttleRegistry.GetOrCreate(info.DeviceID),
	})
}
