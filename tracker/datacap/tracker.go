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
	classify         func(string) string
	client           *Client
	logger           log.ContextLogger
	reportInterval   time.Duration
	throttleRegistry *ThrottleRegistry
}

type Options struct {
	TrafficCategories bool     `json:"traffic_categories,omitempty"`
	LanternHosts      []string `json:"lantern_hosts,omitempty"`
	ProbeHosts        []string `json:"probe_hosts,omitempty"`
	URL               string   `json:"url,omitempty"`
	ReportInterval    string   `json:"report_interval,omitempty"`
	HTTPTimeout       string   `json:"http_timeout,omitempty"`
}

func NewDatacapTracker(options Options, logger log.ContextLogger) (*DatacapTracker, error) {
	if options.URL == "" {
		return nil, E.New("datacap url not defined")
	}
	// Parse intervals with defaults
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
	var classify func(string) string
	if options.TrafficCategories {
		classify = newTrafficClassifier(options.LanternHosts, options.ProbeHosts)
	}
	return &DatacapTracker{
		classify:         classify,
		client:           NewClient(options.URL, httpTimeout),
		reportInterval:   reportInterval,
		throttleRegistry: NewThrottleRegistry(),
		logger:           logger,
	}, nil
}

func (t *DatacapTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if !ok {
		// conn is not from a clientcontext-aware client (e.g., not radiance)
		t.logger.Debug("skipping datacap: no client info in context")
		return conn
	}
	if info.IsPro {
		t.logger.Debug("skipping datacap: client is pro ", info.DeviceID)
		return conn
	}
	category := ""
	if t.classify != nil {
		category = t.classify(metadata.Destination.Fqdn)
	}
	return NewConn(ConnConfig{
		TrafficCategory: category,
		Conn:            conn,
		Client:          t.client,
		Logger:          t.logger,
		ClientInfo:      info,
		ReportInterval:  t.reportInterval,
		Throttler:       t.throttleRegistry.GetOrCreate(info.DeviceID),
	})
}
func (t *DatacapTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	info, ok := clientcontext.ClientInfoFromContext(ctx)
	if !ok {
		// conn is not from a clientcontext-aware client (e.g., not radiance)
		t.logger.Debug("skipping datacap: no client info in context")
		return conn
	}
	if info.IsPro {
		t.logger.Debug("skipping datacap: client is pro ", info.DeviceID)
		return conn
	}
	return NewPacketConn(PacketConnConfig{
		TrafficCategories: t.classify != nil,
		Conn:              conn,
		Client:            t.client,
		Logger:            t.logger,
		ClientInfo:        info,
		ReportInterval:    t.reportInterval,
		Throttler:         t.throttleRegistry.GetOrCreate(info.DeviceID),
	})
}
