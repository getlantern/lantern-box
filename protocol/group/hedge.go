package group

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
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
	// hedgeFailoverDialTimeout bounds the dial to Then when a connection
	// moves. The original dial's context may be gone by then.
	hedgeFailoverDialTimeout = 15 * time.Second
)

func RegisterHedge(registry *outbound.Registry) {
	outbound.Register[option.HedgeOutboundOptions](registry, constant.TypeHedge, NewHedge)
}

// Hedge sends each connection to its First outbound and, if First doesn't
// answer, moves it to Then by replaying the bytes the client sent. Until the
// first reply nothing from First has reached the client, so the client's TLS
// or QUIC session, which runs end to end, cannot tell the two paths apart.
//
// Whether First worked is remembered per site in a cache shared by every hedge
// in the box: a site First failed for goes straight to Then, with no timeout,
// until the failure expires. After a connection commits to First, a stall or
// reset marks the site bad too, so its next connection uses Then.
//
// Bandit probes (see [lAdapter.ContextWithProbe]) measure the proxy behind
// the hedge, so they always go straight to Then and teach the cache nothing.
type Hedge struct {
	outbound.Adapter
	outboundMgr adapter.OutboundManager
	logger      logger.ContextLogger
	firstTag    string
	thenTag     string
	cfg         hedgeConfig
	cache       *hedgeCache
}

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
	return &Hedge{
		Adapter:     outbound.NewAdapter(constant.TypeHedge, tag, []string{N.NetworkTCP, N.NetworkUDP}, []string{options.First, options.Then}),
		outboundMgr: service.FromContext[adapter.OutboundManager](ctx),
		logger:      logger,
		firstTag:    options.First,
		thenTag:     options.Then,
		cfg:         cfg,
		cache:       sharedHedgeCache(ctx),
	}, nil
}

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
	if lAdapter.IsProbe(ctx) {
		return nil, hedgeKey{}, false
	}
	first, err := h.outbound(h.firstTag)
	if err != nil || !slices.Contains(first.Network(), network) {
		return nil, hedgeKey{}, false
	}
	key := hedgeKeyFor(ctx, network, destination)
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
		return bufio.NewBindPacketConn(conn, destination), nil
	}
	then, err := h.outbound(h.thenTag)
	if err != nil {
		return nil, err
	}
	first, key, ok := h.useFirst(ctx, N.NetworkTCP, destination)
	if !ok {
		return then.DialContext(ctx, network, destination)
	}
	conn, err := first.DialContext(ctx, network, destination)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		h.markBad(ctx, key, err)
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
	conn, err := first.ListenPacket(ctx, destination)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		h.markBad(ctx, key, err)
		return then.ListenPacket(ctx, destination)
	}
	return newHedgePacket(h, ctx, key, destination, conn), nil
}

// failoverContext detaches from the dial's context, which may end once the
// dial returns, and bounds the dial to Then.
func failoverContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), hedgeFailoverDialTimeout)
}
