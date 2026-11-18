package group

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	lAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

const (
	defaultHedgeCommitTimeout = time.Second
	defaultHedgeFailureTTL    = 5 * time.Minute
	defaultHedgeSuccessTTL    = time.Hour
	defaultHedgeMaxHeldBytes  = 64 << 10
	// hedgeMaxHeldPackets bounds the datagrams kept for replay; a QUIC
	// handshake's first flight is a handful.
	hedgeMaxHeldPackets = 64
)

// hedgeFailoverDialTimeout bounds the dial to Then when a connection moves,
// and the replay to it. The original dial's context may be gone by then. A
// variable so tests can shorten it.
var hedgeFailoverDialTimeout = 15 * time.Second

func RegisterHedge(registry *outbound.Registry) {
	outbound.Register[option.HedgeOutboundOptions](registry, constant.TypeHedge, NewHedge)
}

// Hedge sends each connection to its First outbound and, if First doesn't
// answer, moves it to Then by replaying the bytes the client sent. Until the
// first reply nothing from First has reached the client, so the client's TLS
// or QUIC session, which runs end to end, cannot tell the two paths apart.
// Only connections that open with a TLS or QUIC handshake are replayed (see
// hedgeLeg); the rest stay on First.
//
// Whether First worked is remembered per site in a cache shared by every hedge
// in the box: a site First failed for goes straight to Then, with no timeout,
// until the failure expires. After a connection commits to First, a stall or
// reset marks the site bad too, so its next connection uses Then.
//
// Bandit probes (see [lAdapter.ContextWithProbe]) measure the proxy behind
// the hedge, so they always go straight to Then and teach the cache nothing.
//
// A hedge is a [lAdapter.Switchable]: switched off, it sends every new
// connection straight to Then and leaves the cache alone, so a feature flag
// can stop proxyless use without rebuilding the tunnel.
type Hedge struct {
	outbound.Adapter
	outboundMgr adapter.OutboundManager
	logger      logger.ContextLogger
	firstTag    string
	thenTag     string
	cfg         hedgeConfig
	cache       *hedgeCache
	enabled     atomic.Bool
}

var _ lAdapter.Switchable = (*Hedge)(nil)

type hedgeConfig struct {
	commitTimeout time.Duration
	failureTTL    time.Duration
	successTTL    time.Duration
	maxHeldBytes  int
}

func NewHedge(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.HedgeOutboundOptions) (adapter.Outbound, error) {
	if options.First == "" || options.Then == "" {
		return nil, errors.New("hedge: first and then outbound tags are required")
	}
	if options.First == options.Then {
		return nil, fmt.Errorf("hedge: first and then must differ, both are %q", options.First)
	}
	cfg := hedgeConfig{
		commitTimeout: time.Duration(options.CommitTimeout),
		failureTTL:    time.Duration(options.FailureTTL),
		successTTL:    time.Duration(options.SuccessTTL),
		maxHeldBytes:  options.MaxHeldBytes,
	}
	if cfg.commitTimeout <= 0 {
		cfg.commitTimeout = defaultHedgeCommitTimeout
	}
	if cfg.failureTTL <= 0 {
		cfg.failureTTL = defaultHedgeFailureTTL
	}
	if cfg.successTTL <= 0 {
		cfg.successTTL = defaultHedgeSuccessTTL
	}
	if cfg.maxHeldBytes <= 0 {
		cfg.maxHeldBytes = defaultHedgeMaxHeldBytes
	}
	h := &Hedge{
		Adapter:     outbound.NewAdapter(constant.TypeHedge, tag, []string{N.NetworkTCP, N.NetworkUDP}, []string{options.First, options.Then}),
		outboundMgr: service.FromContext[adapter.OutboundManager](ctx),
		logger:      logger,
		firstTag:    options.First,
		thenTag:     options.Then,
		cfg:         cfg,
		cache:       sharedHedgeCache(ctx),
	}
	h.enabled.Store(!options.Disabled)
	return h, nil
}

// SetEnabled switches the hedge on or off for new connections.
func (h *Hedge) SetEnabled(enabled bool) {
	if h.enabled.Swap(enabled) != enabled {
		h.logger.Info("hedge ", h.Tag(), " switched ", map[bool]string{true: "on", false: "off"}[enabled])
	}
}

// Enabled reports whether new connections may try First.
func (h *Hedge) Enabled() bool { return h.enabled.Load() }

func (h *Hedge) Start() error {
	for _, tag := range []string{h.firstTag, h.thenTag} {
		if _, loaded := h.outboundMgr.Outbound(tag); !loaded {
			return fmt.Errorf("hedge: missing outbound %s", tag)
		}
	}
	return nil
}

// InterfaceUpdated forgets what the cache learned: what is blocked depends on
// the network.
func (h *Hedge) InterfaceUpdated(context.Context) {
	h.cache.clear()
}

// outbound looks the tag up on every dial, since groups can be replaced at
// runtime.
func (h *Hedge) outbound(tag string) (adapter.Outbound, error) {
	o, loaded := h.outboundMgr.Outbound(tag)
	if !loaded {
		return nil, fmt.Errorf("hedge: outbound %s not found", tag)
	}
	return o, nil
}

// useFirst reports whether a dial should try First, and the key its result
// is remembered by.
func (h *Hedge) useFirst(ctx context.Context, network string, destination M.Socksaddr) (adapter.Outbound, hedgeKey, bool) {
	if lAdapter.IsProbe(ctx) || !h.enabled.Load() {
		return nil, hedgeKey{}, false
	}
	first, err := h.outbound(h.firstTag)
	if err != nil || !slices.Contains(first.Network(), network) {
		return nil, hedgeKey{}, false
	}
	key := hedgeKeyFor(ctx, network, destination)
	key.gen = h.cache.generation()
	if h.cache.verdict(key) == hedgeBad {
		return nil, key, false
	}
	return first, key, true
}

func (h *Hedge) markBad(ctx context.Context, key hedgeKey, why any) {
	h.logger.DebugContext(ctx, "hedge: ", h.firstTag, " failed for ", key.site, "/", key.network, " (", why, "), using ", h.thenTag)
	h.cache.mark(key, hedgeBad, h.cfg.failureTTL)
}

func (h *Hedge) markGood(key hedgeKey) {
	h.cache.mark(key, hedgeGood, h.cfg.successTTL)
}

func (h *Hedge) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) == N.NetworkUDP {
		conn, err := h.ListenPacket(ctx, destination)
		if err != nil {
			return nil, err
		}
		// A plain UDP socket, which Then may return when First is skipped,
		// takes only *net.UDPAddr; a domain name is kept for conns that
		// resolve it.
		var remote net.Addr = destination
		if destination.IsIP() {
			remote = destination.UDPAddr()
		}
		return bufio.NewBindPacketConn(conn, remote), nil
	}
	then, err := h.outbound(h.thenTag)
	if err != nil {
		return nil, err
	}
	first, key, ok := h.useFirst(ctx, N.NetworkTCP, destination)
	if !ok {
		return then.DialContext(ctx, network, destination)
	}
	dialCtx, timedOut, stop := h.firstDialContext(ctx)
	conn, err := first.DialContext(dialCtx, network, destination)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		if !timedOut() {
			h.markBad(ctx, key, err)
		}
		return then.DialContext(ctx, network, destination)
	}
	return newHedgeStream(h, ctx, key, network, destination, conn), nil
}

func (h *Hedge) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	then, err := h.outbound(h.thenTag)
	if err != nil {
		return nil, err
	}
	first, key, ok := h.useFirst(ctx, N.NetworkUDP, destination)
	if !ok {
		return then.ListenPacket(ctx, destination)
	}
	dialCtx, timedOut, stop := h.firstDialContext(ctx)
	conn, err := first.ListenPacket(dialCtx, destination)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		if !timedOut() {
			h.markBad(ctx, key, err)
		}
		return then.ListenPacket(ctx, destination)
	}
	return newHedgePacket(h, ctx, key, destination, conn), nil
}

// firstDialContext bounds First's dial by the commit timeout, so a slow
// dial, such as the outline outbound's first strategy search, falls back to
// Then instead of holding the connection. A dial that ran out of time is
// slow, not blocked, so it doesn't mark the site bad.
//
// The context is cancelled by that timeout or by ctx, and never once the dial
// has returned, since some outbounds tie a connection's life to the context
// it was dialed with. It hangs off ctx through context.AfterFunc rather than
// WithCancel, so stop can detach it: a child that is never cancelled would
// stay registered on the inbound's long-lived context for every connection.
// stop must be called once the dial returns.
func (h *Hedge) firstDialContext(ctx context.Context) (dialCtx context.Context, timedOut func() bool, stop func()) {
	dialCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	detach := context.AfterFunc(ctx, cancel)
	var fired atomic.Bool
	timer := time.AfterFunc(h.cfg.commitTimeout, func() {
		fired.Store(true)
		cancel()
	})
	return dialCtx, fired.Load, func() {
		timer.Stop()
		detach()
	}
}

// failoverContext detaches from the dial's context, which may end once the
// dial returns, and bounds the dial to Then.
func failoverContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), hedgeFailoverDialTimeout)
}
