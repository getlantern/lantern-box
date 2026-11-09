// Package outline implements the smart dialer outbound using the outline-sdk package.
// You can find more details here: https://github.com/Jigsaw-Code/outline-sdk/tree/v0.0.18/x/smart
package outline

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/x/smart"
	"gopkg.in/yaml.v3"

	C "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

// Outbound implements the smart dialer outbound from outline sdk
type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	// createDialer runs a strategy search. A failed search is not cached,
	// because it can fail transiently, e.g. before the network is up.
	createDialer func() (transport.StreamDialer, error)

	mu     sync.Mutex
	dialer transport.StreamDialer
	search *dialerSearch
}

// dialerSearch is one in-flight createDialer call, shared by every dial that
// arrives before it finishes. dialer and err are set before done is closed.
type dialerSearch struct {
	done   chan struct{}
	dialer transport.StreamDialer
	err    error
}

// smartConfig is the config document accepted by
// [smart.StrategyFinder.NewDialer], which rejects unknown keys.
type smartConfig struct {
	DNS []option.DNSEntryConfig `yaml:"dns,omitempty"`
	TLS []string                `yaml:"tls,omitempty"`
}

func marshalSmartConfig(options option.OutboundOutlineOptions) ([]byte, error) {
	return yaml.Marshal(smartConfig{DNS: options.DNSResolvers, TLS: options.TLS})
}

// RegisterOutbound registers the outline outbound to the registry
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register(registry, C.TypeOutline, NewOutbound)
}

// NewOutbound creates a proxyless outbond that uses the proxyless transport
// for dialing
func NewOutbound(ctx context.Context, router adapter.Router, log log.ContextLogger, tag string, options option.OutboundOutlineOptions) (adapter.Outbound, error) {
	sDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}

	outboundSD := &outboundDialer{
		dialer: sDialer,
		logger: log,
	}

	if options.TestTimeout == "" {
		options.TestTimeout = "0s"
	}
	timeout, err := time.ParseDuration(options.TestTimeout)
	if err != nil {
		return nil, err
	}

	strategyFinder := &smart.StrategyFinder{
		TestTimeout:  timeout,
		LogWriter:    &logWriter{log},
		StreamDialer: outboundSD,
		PacketDialer: outboundSD,
	}

	yamlOptions, err := marshalSmartConfig(options)
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(C.TypeOutline, tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:  log,
		// During the dialer creation the strategy finder try to use the stream dialer
		// for resolving the domains. We can't create the smart dialer during the
		// outbound initialization because there wouldn't be a tunnel to communicate.
		// So for fixing this issue, the dialer must be created during the DialContext call.
		createDialer: func() (transport.StreamDialer, error) {
			return strategyFinder.NewDialer(ctx, options.Domains, yamlOptions)
		},
	}, nil
}

// DialContext extracts the metadata domain, add the destination to the context
// and use the proxyless dialer for sending the request
func (o *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	d, err := o.getDialer(ctx)
	if err != nil {
		return nil, err
	}

	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination

	return d.DialStream(ctx, fmt.Sprintf("%s:%d", md.Domain, destination.Port))
}

// getDialer returns the smart dialer, joining the in-flight search or starting
// one if none is running. The search outlives ctx, which only bounds the wait.
func (o *Outbound) getDialer(ctx context.Context) (transport.StreamDialer, error) {
	o.mu.Lock()
	if o.dialer != nil {
		d := o.dialer
		o.mu.Unlock()
		return d, nil
	}
	s := o.search
	if s == nil {
		s = &dialerSearch{done: make(chan struct{})}
		o.search = s
		go o.runSearch(s)
	}
	o.mu.Unlock()

	select {
	case <-s.done:
		return s.dialer, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (o *Outbound) runSearch(s *dialerSearch) {
	s.dialer, s.err = o.createDialer()
	o.mu.Lock()
	if s.err == nil {
		o.dialer = s.dialer
	}
	o.search = nil
	o.mu.Unlock()
	close(s.done)
}

// ListenPacket isn't implemented
func (o *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

// wrapper around sing-box's network.Dialer to implement streamDialer interface to pass to a
// stream dialer as innerSD
type outboundDialer struct {
	dialer network.Dialer
	logger log.ContextLogger
}

func (s *outboundDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	destination := metadata.ParseSocksaddr(addr)
	conn, err := s.dialer.DialContext(ctx, network.NetworkTCP, destination)
	if err != nil {
		return nil, err
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("expected *net.TCPConn, got %T", conn)
	}
	return tcpConn, nil
}

func (s *outboundDialer) DialPacket(ctx context.Context, addr string) (net.Conn, error) {
	destination := metadata.ParseSocksaddr(addr)
	conn, err := s.dialer.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("failed to assert connection as *net.UDPConn")
	}
	return udpConn, nil
}

type logWriter struct {
	logger log.ContextLogger
}

func (w *logWriter) Write(p []byte) (int, error) {
	if w.logger != nil {
		w.logger.Debug(string(p))
	}
	return len(p), nil
}
