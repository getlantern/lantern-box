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
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"golang.getoutline.org/sdk/transport"
	"golang.getoutline.org/sdk/x/configurl"
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
	// sites gives each site a dialer of its own once its search succeeds; nil
	// when the per_site option is false.
	sites *siteDialers

	mu     sync.Mutex
	dialer transport.StreamDialer
	search *dialerSearch

	// packetListener serves UDP; nil when the udp option is unset.
	packetListener transport.PacketListener
}

// dialerSearch is one in-flight createDialer call, shared by every dial that
// arrives before it finishes. dialer and err are set before done is closed.
type dialerSearch struct {
	done   chan struct{}
	dialer transport.StreamDialer
	err    error
}

// smartConfig is the config document accepted by [smart.Finder.NewDialer],
// which rejects unknown keys.
type smartConfig struct {
	DNS      []option.DNSEntryConfig `yaml:"dns,omitempty"`
	TLS      []string                `yaml:"tls,omitempty"`
	Fallback []string                `yaml:"fallback,omitempty"`
}

func marshalSmartConfig(options option.OutboundOutlineOptions) ([]byte, error) {
	return yaml.Marshal(smartConfig{DNS: options.DNSResolvers, TLS: options.TLS, Fallback: options.Fallback})
}

// RegisterOutbound registers the outline outbound to the registry
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register(registry, C.TypeOutline, NewOutbound)
}

// NewOutbound creates a proxyless outbound that uses the proxyless transport
// for dialing
func NewOutbound(ctx context.Context, router adapter.Router, log log.ContextLogger, tag string, options option.OutboundOutlineOptions) (adapter.Outbound, error) {
	sDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}
	packetDialer := N.Dialer(sDialer)
	if options.UDP != nil && service.FromContext[adapter.NetworkManager](ctx) != nil {
		// UDP destinations can be domain names, as from a SOCKS5 or HTTP
		// inbound, so the UDP base resolves them the way the direct outbound
		// does. TCP keeps sDialer: the smart dialer picks its own resolver.
		// Resolving needs the box's network manager, which a bare context
		// in tests lacks.
		packetDialer, err = dialer.NewWithOptions(dialer.Options{
			Context:        ctx,
			Options:        options.DialerOptions,
			RemoteIsDomain: true,
		})
		if err != nil {
			return nil, err
		}
	}
	registry := newRegistry(&outboundDialer{dialer: sDialer, packetDialer: packetDialer, logger: log})

	var testTimeout time.Duration
	if options.TestTimeout != "" {
		testTimeout, err = time.ParseDuration(options.TestTimeout)
		if err != nil {
			return nil, err
		}
	}
	newFinder := func(cache smart.StrategyResultCache) (smart.Finder, error) {
		c := smart.NewFinderConfig().
			SetConfigURLRegistry(registry).
			SetLogWriter(&logWriter{log})
		if testTimeout > 0 {
			c.SetTestTimeout(testTimeout)
		}
		if cache != nil {
			c.SetCache(cache)
		}
		return c.New()
	}
	var cache smart.StrategyResultCache
	if options.StrategyCachePath != "" {
		cache = strategyCacheFor(options.StrategyCachePath, log)
	}
	finder, err := newFinder(cache)
	if err != nil {
		return nil, err
	}

	yamlOptions, err := marshalSmartConfig(options)
	if err != nil {
		return nil, err
	}

	networks := []string{N.NetworkTCP}
	var packetListener transport.PacketListener
	if options.UDP != nil {
		packetListener, err = configurl.New[transport.PacketListener](ctx, registry, *options.UDP)
		if err != nil {
			return nil, fmt.Errorf("outline: udp config %q: %w", *options.UDP, err)
		}
		networks = append(networks, N.NetworkUDP)
	}
	var sites *siteDialers
	if options.PerSite == nil || *options.PerSite {
		// The finder keeps its winner under one cache key, so sites sharing
		// the outbound's cache would overwrite each other's; site searches
		// go uncached.
		siteFinder, err := newFinder(nil)
		if err != nil {
			return nil, err
		}
		sites = newSiteDialers(ctx, maxSites, func(ctx context.Context, _, host string) (transport.StreamDialer, error) {
			return siteFinder.NewDialer(ctx, []string{host}, yamlOptions)
		})
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(C.TypeOutline, tag, networks, options.DialerOptions),
		logger:  log,
		// During the dialer creation the strategy finder try to use the stream dialer
		// for resolving the domains. We can't create the smart dialer during the
		// outbound initialization because there wouldn't be a tunnel to communicate.
		// So for fixing this issue, the dialer must be created during the DialContext call.
		createDialer: func() (transport.StreamDialer, error) {
			return finder.NewDialer(ctx, options.Domains, yamlOptions)
		},
		sites:          sites,
		packetListener: packetListener,
	}, nil
}

// newRegistry returns the default config URL registry with every base
// replaced by d, so strategies, fallbacks and the UDP listener all dial
// through the outbound's own sing-box dialer and its socket protection.
func newRegistry(d *outboundDialer) configurl.MutableRegistry {
	registry := configurl.NewDefaultRegistry()
	configurl.RegisterBase(registry, func(context.Context, configurl.Registry, *configurl.Config) (transport.StreamDialer, error) {
		return d, nil
	})
	configurl.RegisterBase(registry, func(context.Context, configurl.Registry, *configurl.Config) (transport.PacketDialer, error) {
		return d, nil
	})
	configurl.RegisterBase(registry, func(context.Context, configurl.Registry, *configurl.Config) (transport.PacketListener, error) {
		return d, nil
	})
	return registry
}

// DialContext dials destination through the smart dialer, which needs a
// hostname for its DNS and TLS strategies. It uses the destination's own
// hostname, else the inbound metadata's domain (sniffed, or from a reverse DNS
// mapping) when the destination is an IP, else the IP. Callers that dial the
// outbound directly, such as a remote rule-set's download_detour, carry no
// metadata. A nested dial with an IP destination, such as a DNS server using
// this outbound as its detour, can still inherit a parent connection's
// unrelated domain.
func (o *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) == N.NetworkUDP {
		conn, err := o.ListenPacket(ctx, destination)
		if err != nil {
			return nil, err
		}
		// A plain UDP socket takes only *net.UDPAddr; a domain name is
		// kept for the resolving dialer to map.
		var remote net.Addr = destination
		if destination.IsIP() {
			remote = destination.UDPAddr()
		}
		return bufio.NewBindPacketConn(conn, remote), nil
	}

	ctx, md := adapter.ExtendContext(ctx)
	var addr metadata.Socksaddr
	switch {
	case destination.IsDomain():
		addr = metadata.Socksaddr{Fqdn: destination.Fqdn, Port: destination.Port}
	case md.Domain != "" && destination.Addr.IsValid():
		addr = metadata.Socksaddr{Fqdn: md.Domain, Port: destination.Port}
	case destination.Addr.IsValid():
		addr = destination.Unwrap()
	default:
		return nil, fmt.Errorf("outline: destination %q has no host", destination.String())
	}

	var d transport.StreamDialer
	if o.sites != nil && addr.IsFqdn() {
		d = o.sites.dialerFor(addr.Fqdn)
	}
	if d == nil {
		var err error
		if d, err = o.getDialer(ctx); err != nil {
			return nil, err
		}
	}

	md.Outbound = o.Tag()
	md.Destination = destination

	return d.DialStream(ctx, addr.String())
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

// ListenPacket returns a UDP socket built from the udp option's config URL,
// such as quicprelude over the outbound's dialer. Without the option the
// outbound is TCP-only.
func (o *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	if o.packetListener == nil {
		return nil, os.ErrInvalid
	}
	ctx, md := adapter.ExtendContext(ctx)
	md.Outbound = o.Tag()
	md.Destination = destination
	o.logger.InfoContext(ctx, "outbound packet connection to ", destination)
	return o.packetListener.ListenPacket(contextWithDestination(ctx, destination))
}

type destinationKey struct{}

// contextWithDestination carries the destination to the base listener, which
// sing-box's dialer needs to pick the address family and bind interface.
// transport.PacketListener has no destination parameter of its own.
func contextWithDestination(ctx context.Context, destination metadata.Socksaddr) context.Context {
	return context.WithValue(ctx, destinationKey{}, destination)
}

// ListenPacket implements transport.PacketListener for the registry base.
func (s *outboundDialer) ListenPacket(ctx context.Context) (net.PacketConn, error) {
	destination, _ := ctx.Value(destinationKey{}).(metadata.Socksaddr)
	return s.packetDialer.ListenPacket(ctx, destination)
}

// wrapper around sing-box's N.Dialer to implement streamDialer interface to pass to a
// stream dialer as innerSD
type outboundDialer struct {
	dialer N.Dialer
	// packetDialer opens the UDP listener's sockets; it resolves domain
	// names when the box can.
	packetDialer N.Dialer
	logger       log.ContextLogger
}

func (s *outboundDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	destination := metadata.ParseSocksaddr(addr)
	conn, err := s.dialer.DialContext(ctx, N.NetworkTCP, destination)
	if err != nil {
		return nil, err
	}
	return newStreamConn(conn)
}

// newStreamConn makes a StreamConn of a TCP connection from sing-box's dialer.
// Inside a box the dialer wraps each connection to track it, so the socket is
// found through the wrapper. The wrapper is kept, so closing the stream still
// stops tracking it.
func newStreamConn(conn net.Conn) (transport.StreamConn, error) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		return tcp, nil
	}
	tcp, ok := common.Cast[*net.TCPConn](conn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("expected a TCP connection, got %T", conn)
	}
	return &streamConn{Conn: conn, tcp: tcp}, nil
}

// streamConn is a tracked TCP connection. NetConn gives strategies that set
// socket options, such as disorder, the socket itself.
type streamConn struct {
	net.Conn
	tcp *net.TCPConn
}

func (c *streamConn) CloseRead() error  { return c.tcp.CloseRead() }
func (c *streamConn) CloseWrite() error { return c.tcp.CloseWrite() }
func (c *streamConn) NetConn() net.Conn { return c.tcp }

// DialPacket returns a UDP connection to addr. Any net.Conn will do, so a
// tracked one from inside a box is returned as is.
func (s *outboundDialer) DialPacket(ctx context.Context, addr string) (net.Conn, error) {
	return s.dialer.DialContext(ctx, N.NetworkUDP, metadata.ParseSocksaddr(addr))
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
