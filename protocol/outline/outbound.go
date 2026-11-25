// Package outline implements the smart dialer outbound using the outline-sdk package.
// You can find more details here: https://github.com/Jigsaw-Code/outline-sdk/tree/v0.0.18/x/smart
package outline

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
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
	// when per-site searches are off.
	sites *siteDialers
	// cancel ends the site searches.
	cancel context.CancelFunc

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
	var (
		sites     *siteDialers
		searchCtx context.Context
		cancel    context.CancelFunc
	)
	siteOptions, perSite := perSiteOptions(options)
	allowed, rejected, anySite := perSiteDomains(options.PerSiteDomains)
	if perSite && len(rejected) > 0 {
		quoted := make([]string, len(rejected))
		for i, r := range rejected {
			quoted[i] = strconv.Quote(r)
		}
		log.Warn("outline: per_site_domains entries naming no site, ignored: ", strings.Join(quoted, ", "))
	}
	if perSite && !anySite {
		log.Warn("outline: per_site_domains names no site, so per-site searches are off")
	}
	if !perSite && len(options.PerSiteDomains) > 0 && (options.PerSite == nil || *options.PerSite) {
		log.Warn("outline: per_site_domains is set, but per-site searches need a https or tls resolver")
	}
	if perSite && anySite {
		siteYAML, err := marshalSmartConfig(siteOptions)
		if err != nil {
			return nil, err
		}
		// The finder keeps its winner under one cache key, so sites sharing
		// the outbound's cache would overwrite each other's; site searches
		// go uncached.
		siteFinder, err := newFinder(nil)
		if err != nil {
			return nil, err
		}
		// The box's context isn't cancelled when it closes, so the
		// searches get their own, which Close cancels.
		searchCtx, cancel = context.WithCancel(ctx)
		sites = newSiteDialers(searchCtx, maxSites, allowed, func(ctx context.Context, _, host string) (transport.StreamDialer, error) {
			return siteFinder.NewDialer(ctx, []string{host}, siteYAML)
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
		cancel:         cancel,
		packetListener: packetListener,
	}, nil
}

// perSiteOptions returns the options for site searches, and false when
// per-site searches are off. A search races every resolver for the site's
// name, where a dial asks only the one its search chose, so site searches
// use only the encrypted resolvers: a plaintext one would see, and show the
// network, every site the user reaches. With none, they're off. An entry that
// also names a plaintext resolver keeps only its encrypted ones, since the
// finder would prefer the system resolver.
func perSiteOptions(options option.OutboundOutlineOptions) (option.OutboundOutlineOptions, bool) {
	if options.PerSite != nil && !*options.PerSite {
		return options, false
	}
	var encrypted []option.DNSEntryConfig
	for _, e := range options.DNSResolvers {
		if e.HTTPS != nil || e.TLS != nil {
			encrypted = append(encrypted, option.DNSEntryConfig{HTTPS: e.HTTPS, TLS: e.TLS})
		}
	}
	if len(encrypted) == 0 {
		return options, false
	}
	options.DNSResolvers = encrypted
	// A fallback dials through the plain base dialer, which resolves the
	// site's name with the system resolver, so a site search leaves them out.
	// A site with no strategy of its own uses the outbound's dialer, which
	// keeps them.
	options.Fallback = nil
	return options, true
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

	md.Outbound = o.Tag()
	md.Destination = destination

	if o.sites != nil && addr.IsFqdn() {
		if d := o.sites.dialerFor(addr.Fqdn, addr.Port); d != nil {
			return o.dialSite(ctx, d, addr)
		}
	}
	return o.dialOutbound(ctx, addr.String())
}

func (o *Outbound) dialOutbound(ctx context.Context, addr string) (transport.StreamConn, error) {
	d, err := o.getDialer(ctx)
	if err != nil {
		return nil, err
	}
	return d.DialStream(ctx, addr)
}

type dialResult struct {
	conn transport.StreamConn
	err  error
	site bool
}

// dialLeg is one dial of a race. Its context ends with the caller's until the
// leg is settled, and is never cancelled once it wins, since the caller's
// isn't once its dial has returned. It keeps the caller's deadline, which the
// SDK's DNS-over-TLS resolver relies on to bound its reads.
type dialLeg struct {
	cancel context.CancelFunc
	detach func() bool
}

func startLeg(ctx context.Context, site bool, results chan<- dialResult, dial func(context.Context) (transport.StreamConn, error)) *dialLeg {
	var (
		legCtx context.Context
		cancel context.CancelFunc
	)
	if deadline, ok := ctx.Deadline(); ok {
		legCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline)
	} else {
		legCtx, cancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	l := &dialLeg{cancel: cancel, detach: context.AfterFunc(ctx, cancel)}
	go func() {
		conn, err := dial(legCtx)
		results <- dialResult{conn, err, site}
	}()
	return l
}

// dialSite dials addr through d, its site's dialer. The outbound's dialer
// dials too once d fails or hasn't connected within siteDialHeadStart, as it
// would have without the site's search, and the first connection wins. A site
// dial that fails, or loses to the outbound's, counts against the site, so a
// strategy that stalls is dropped like one that fails. A dial whose caller
// cancelled it says nothing about the strategy, and isn't counted.
func (o *Outbound) dialSite(ctx context.Context, d *siteDialer, addr metadata.Socksaddr) (net.Conn, error) {
	host, target := addr.Fqdn, addr.String()
	results := make(chan dialResult, 2)
	legs := map[bool]*dialLeg{true: startLeg(ctx, true, results, func(ctx context.Context) (transport.StreamConn, error) {
		return d.DialStream(ctx, target)
	})}
	startOutbound := func() {
		if legs[false] == nil && ctx.Err() == nil {
			legs[false] = startLeg(ctx, false, results, func(ctx context.Context) (transport.StreamConn, error) {
				return o.dialOutbound(ctx, target)
			})
		}
	}
	// release cancels every leg but keep, which may be nil, and closes any
	// connection the n still running return.
	release := func(keep *dialLeg, n int) {
		for _, l := range legs {
			if l != keep {
				l.detach()
				l.cancel()
			}
		}
		go func() {
			for range n {
				if late := <-results; late.conn != nil {
					late.conn.Close()
				}
			}
		}()
	}
	timer := time.NewTimer(siteDialHeadStart)
	defer timer.Stop()

	var lastErr error
	for pending := 1; pending > 0; {
		select {
		case <-ctx.Done():
			// A leg that ignores its context mustn't hold the caller.
			release(nil, pending)
			return nil, ctx.Err()
		case <-timer.C:
			if legs[false] == nil {
				startOutbound()
				if legs[false] != nil {
					pending++
				}
			}
			continue
		case r := <-results:
			pending--
			legs[r.site].detach()
			if r.err == nil {
				if r.site {
					o.sites.dialed(host, d, nil)
				} else if pending > 0 && ctx.Err() == nil {
					// The site's dial is still running: it lost.
					o.sites.dialed(host, d, errSiteDialSlow)
				}
				release(legs[r.site], pending)
				return r.conn, nil
			}
			legs[r.site].cancel()
			lastErr = r.err
			if r.site {
				if ctx.Err() == nil {
					o.sites.dialed(host, d, r.err)
				}
				// The site's strategy failed; the outbound's may still work.
				if legs[false] == nil {
					startOutbound()
					if legs[false] != nil {
						pending++
					}
				}
			}
		}
	}
	return nil, lastErr
}

// Close ends the outbound's site searches.
func (o *Outbound) Close() error {
	if o.cancel != nil {
		o.cancel()
	}
	return nil
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
