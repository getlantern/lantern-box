// Package banditprobe answers bandit callback probes on the proxy itself.
//
// A censor can freeze a flow after a few kilobytes while still letting the
// small request through. The API cannot see that from where it sits, because
// its TCP peer is a load balancer. The proxy is the sender on the leg that
// freezes, so it can watch the client acknowledge (or not acknowledge) a body
// large enough to cross the freeze point, and report what it saw.
package banditprobe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

const (
	defaultBodySize     = 64 * 1024
	defaultStallTimeout = 2500 * time.Millisecond
	defaultMaxWait      = 10 * time.Second
	maxMaxWait          = 15 * time.Second
	requestReadTimeout  = 10 * time.Second
	maxRequestBytes     = 8 << 10
	callbackTimeout     = 10 * time.Second
	bodyPoolSize        = 1 << 20
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.BanditProbeOutboundOptions](registry, constant.TypeBanditProbe, NewOutbound)
}

var _ adapter.ConnectionHandlerEx = (*Outbound)(nil)

// Outbound only handles connections the router hands it; it never dials.
type Outbound struct {
	outbound.Adapter
	responder *responder
}

func NewOutbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.BanditProbeOutboundOptions) (adapter.Outbound, error) {
	cfg, err := newConfig(options)
	if err != nil {
		return nil, err
	}
	pool := make([]byte, bodyPoolSize)
	if _, err := rand.Read(pool); err != nil {
		return nil, fmt.Errorf("filling body pool: %w", err)
	}
	r := &responder{
		cfg:        cfg,
		logger:     lg,
		pool:       pool,
		readState:  readSendState,
		httpClient: &http.Client{Timeout: callbackTimeout},
		now:        time.Now,
	}
	return &Outbound{
		Adapter:   outbound.NewAdapter(constant.TypeBanditProbe, tag, []string{N.NetworkTCP}, nil),
		responder: r,
	}, nil
}

func newConfig(options option.BanditProbeOutboundOptions) (config, error) {
	if options.CallbackURL == "" {
		return config{}, errors.New("banditprobe: callback_url is required")
	}
	u, err := url.Parse(options.CallbackURL)
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return config{}, fmt.Errorf("banditprobe: callback_url must be an https URL, got %q", options.CallbackURL)
	}
	cfg := config{
		callbackURL:   u,
		bodySize:      options.BodySize,
		stallTimeout:  time.Duration(options.StallTimeout),
		maxWait:       time.Duration(options.MaxWait),
		reportStalled: options.ReportStalled,
	}
	if cfg.bodySize <= 0 {
		cfg.bodySize = defaultBodySize
	}
	if cfg.bodySize > bodyPoolSize {
		return config{}, fmt.Errorf("banditprobe: body_size %d exceeds %d", cfg.bodySize, bodyPoolSize)
	}
	if cfg.stallTimeout <= 0 {
		cfg.stallTimeout = defaultStallTimeout
	}
	if cfg.maxWait <= 0 {
		cfg.maxWait = defaultMaxWait
	}
	if cfg.maxWait > maxMaxWait {
		return config{}, fmt.Errorf("banditprobe: max_wait %s exceeds %s", cfg.maxWait, maxMaxWait)
	}
	return cfg, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return nil, syscall.EPERM
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, syscall.EPERM
}

func (o *Outbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	go func() {
		err := o.responder.serve(ctx, conn)
		conn.Close()
		if onClose != nil {
			onClose(err)
		}
	}()
}
