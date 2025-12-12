package datacap

import (
	"context"
	"net"
	"time"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

var _ (adapter.ConnectionTracker) = (*DatacapTracker)(nil)

type DatacapTracker struct {
	client           *Client
	logger           log.ContextLogger
	reportInterval   time.Duration
	enableThrottling bool
	throttleSpeed    int64
}

type Options struct {
	URL              string `json:"url,omitempty"`
	ReportInterval   string `json:"report_interval,omitempty"`
	HTTPTimeout      string `json:"http_timeout,omitempty"`
	EnableThrottling bool   `json:"enable_throttling,omitempty"`
	ThrottleSpeed    int64  `json:"throttle_speed,omitempty"`
}

func NewDatacapTracker(options Options, logger log.ContextLogger) (*DatacapTracker, error) {
	if options.URL == "" {
		return nil, E.New("datacap url not defined")
	}
	// Parse intervals with defaults
	reportInterval := 30 * time.Second
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
		client:           NewClient(options.URL, httpTimeout),
		reportInterval:   reportInterval,
		enableThrottling: options.EnableThrottling,
		logger:           logger,
	}, nil
}

func (t *DatacapTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	info := service.PtrFromContext[clientcontext.ClientInfo](ctx)
	if info == nil {
		t.logger.Warn("ClientInfo not found in context")
		return conn
	}
	if info.IsPro {
		return conn
	}
	return NewConn(ConnConfig{
		Conn:             conn,
		Client:           t.client,
		Logger:           t.logger,
		ClientInfo:       info,
		ReportInterval:   t.reportInterval,
		EnableThrottling: t.enableThrottling,
		ThrottleSpeed:    t.throttleSpeed,
	})
}
func (t *DatacapTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	info := service.PtrFromContext[clientcontext.ClientInfo](ctx)
	if info == nil {
		t.logger.Warn("ClientInfo not found in context")
		return conn
	}
	if info.IsPro {
		return conn
	}
	return NewPacketConn(PacketConnConfig{
		Conn:             conn,
		Client:           t.client,
		Logger:           t.logger,
		ClientInfo:       info,
		ReportInterval:   t.reportInterval,
		EnableThrottling: t.enableThrottling,
		ThrottleSpeed:    t.throttleSpeed,
	})
}
