package group

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/constant"
	isync "github.com/getlantern/lantern-box/internal/sync"
	lLog "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/option"
)

func RegisterMutableAutoSelect(registry *outbound.Registry) {
	outbound.Register[option.MutableAutoSelectOutboundOptions](registry, constant.TypeMutableAutoSelect, NewMutableAutoSelect)
}

var (
	_ adapter.MutableOutboundGroup = (*MutableAutoSelect)(nil)
	_ A.OutboundGroup              = (*MutableAutoSelect)(nil)
	_ A.URLTestGroup               = (*MutableAutoSelect)(nil)
	_ A.InterfaceUpdateListener    = (*MutableAutoSelect)(nil)
	_ A.ConnectionHandlerEx        = (*MutableAutoSelect)(nil)
	_ A.PacketConnectionHandlerEx  = (*MutableAutoSelect)(nil)
	_ adapter.URLOverrideSetter    = (*MutableAutoSelect)(nil)
	_ adapter.OutboundChecker      = (*MutableAutoSelect)(nil)
	_ adapter.ExhaustionSignaler   = (*MutableAutoSelect)(nil)
)

const (
	defaultProbeConcurrency = 6
	// Sized for probe delays that exclude connection setup; a larger floor
	// would override switchTolerancePercent at typical delays.
	defaultSwitchTolerance                = 100 * time.Millisecond
	defaultSwitchTolerancePercent         = 30
	defaultConfirmedFailureSwitchCooldown = time.Minute
)

const probeFreshnessWindow = 30 * time.Second

// MutableAutoSelect is the client-side server-selection group.
type MutableAutoSelect struct {
	outbound.Adapter
	ctx         context.Context
	cancel      context.CancelFunc
	outboundMgr A.OutboundManager
	connMgr     A.ConnectionManager
	logger      log.ContextLogger

	access       sync.Mutex
	tags         []string
	members      isync.TypedMap[string, A.Outbound]
	urlOverrides map[string]string
	defaultURL   string
	histories    map[string]*localHistory
	cfg          mutableAutoSelectConfig
	hist         historyParams
	history      adapter.AutoSelectHistoryStorage

	// Scratch buffers reused across calls by rankLocked and splitHealthyForLocked
	// to avoid per-dial allocation; guarded by access, which their callers hold
	// while consuming the returned slice before releasing it.
	scratchPres   []preCandidate
	scratchRanked []rankedCandidate
	scratchSplit  []rankedCandidate

	// Last dial-time tag per network, for switch-tolerance hysteresis.
	// atomic.Value (not access-guarded) so Now() can read without taking
	// the access lock. A stale tag is harmless: the in-pool check in
	// applyStickiness drops it.
	stickyTag struct {
		tcp atomic.Value // string; "" when unset
		udp atomic.Value // string; "" when unset
	}

	probeMembers     map[string]*memberProbeState
	inFlightProbes   map[A.Outbound]*probeAttempt
	probeSlotsUsed   int
	probeSequence    uint64
	nextMembershipID uint64
	probeChanged     chan struct{}
	periodicWaiter   *probeWaiter
	urlWaiters       map[*probeWaiter]struct{}
	ladderWaiter     *probeWaiter
	periodicTimer    *time.Timer
	idleTimer        *time.Timer
	// pauseEpoch is guarded by access.
	pauseEpoch          uint64
	pauseCallbackAccess sync.Mutex
	probePauseManager   pause.Manager
	pauseCallback       *list.Element[pause.Callback]
	lastLadderAt        atomic.Int64
	// lastNetworkAdvanceAt is guarded by access.
	lastNetworkAdvanceAt time.Time
	exhaustionCh         chan struct{}
	// Guarded by access.
	lastConfirmedFailureSwitch struct {
		at          time.Time
		selectedTag string
	}

	// Guarded by access. probing is set while the background loop runs.
	lastActive time.Time
	probing    bool
	// Unix-nano of the most recent non-empty data-plane Read or Write.
	lastDataAt atomic.Int64
	// networkGeneration fences work across interface changes; written under access.
	networkGeneration atomic.Uint64

	started   bool
	closeOnce sync.Once
}

type mutableAutoSelectConfig struct {
	switchTolerance                time.Duration
	switchTolerancePercent         uint32
	activeInterval                 time.Duration
	idleThreshold                  time.Duration
	ladderTotalBudget              time.Duration
	ladderCooldown                 time.Duration
	confirmedFailureSwitchCooldown time.Duration
	dataPlaneIdle                  time.Duration
	dataPlaneFirstResponseTimeout  time.Duration
	dataPlaneProvedRead            uint64
	demoteOnlySelected             bool
	maxPersistedAge                time.Duration
	probeConcurrency               int
}

func resolveMutableAutoSelectOptions(o option.MutableAutoSelectOutboundOptions) (mutableAutoSelectConfig, historyParams) {
	cfg := mutableAutoSelectConfig{
		switchTolerance:                time.Duration(o.SwitchToleranceMs) * time.Millisecond,
		switchTolerancePercent:         defaultSwitchTolerancePercent,
		activeInterval:                 time.Duration(o.BackgroundIntervalSeconds) * time.Second,
		idleThreshold:                  time.Duration(o.IdleThresholdSeconds) * time.Second,
		ladderTotalBudget:              time.Duration(o.LadderTotalBudgetSeconds) * time.Second,
		ladderCooldown:                 time.Duration(o.LadderCooldownSeconds) * time.Second,
		confirmedFailureSwitchCooldown: time.Duration(o.ConfirmedFailureSwitchCooldownSeconds) * time.Second,
		dataPlaneIdle:                  time.Duration(o.DataPlaneIdleSeconds) * time.Second,
		dataPlaneFirstResponseTimeout:  defaultFirstResponseTimeout,
		dataPlaneProvedRead:            uint64(o.DataPlaneProvedReadBytes),
		demoteOnlySelected:             o.DemoteOnlySelectedTag == nil || *o.DemoteOnlySelectedTag,
		maxPersistedAge:                time.Duration(o.MaxPersistedAgeSeconds) * time.Second,
		probeConcurrency:               int(o.ProbeConcurrency),
	}
	if cfg.switchTolerance == 0 {
		cfg.switchTolerance = defaultSwitchTolerance
	}
	if o.SwitchTolerancePercent != nil {
		cfg.switchTolerancePercent = *o.SwitchTolerancePercent
	}
	if cfg.activeInterval == 0 {
		cfg.activeInterval = 180 * time.Second
	}
	if cfg.idleThreshold == 0 {
		cfg.idleThreshold = 900 * time.Second
	}
	if cfg.ladderTotalBudget == 0 {
		cfg.ladderTotalBudget = 15 * time.Second
	}
	if cfg.ladderCooldown == 0 {
		cfg.ladderCooldown = time.Minute
	}
	if cfg.confirmedFailureSwitchCooldown == 0 {
		cfg.confirmedFailureSwitchCooldown = defaultConfirmedFailureSwitchCooldown
	}
	if cfg.dataPlaneIdle == 0 {
		cfg.dataPlaneIdle = defaultDataPlaneIdle
	}
	if cfg.dataPlaneProvedRead == 0 {
		cfg.dataPlaneProvedRead = defaultDataPlaneProvedReadBytes
	}
	if cfg.maxPersistedAge == 0 {
		cfg.maxPersistedAge = defaultMaxPersistedAge
	}
	if cfg.probeConcurrency == 0 {
		cfg.probeConcurrency = defaultProbeConcurrency
	}

	hp := defaultHistoryParams()
	if o.ConsecutiveFailureLimit > 0 {
		hp.consecutiveFailLimit = o.ConsecutiveFailureLimit
	}
	if o.SoftDemoteLimit > 0 {
		hp.softFailLimit = o.SoftDemoteLimit
	}
	if o.UserFailureWindowSeconds > 0 {
		hp.userFailureWindow = time.Duration(o.UserFailureWindowSeconds) * time.Second
	}
	if o.UserFailureDedupeWindowSeconds > 0 {
		hp.userFailureDedupeWindow = time.Duration(o.UserFailureDedupeWindowSeconds) * time.Second
	}
	return cfg, hp
}

func NewMutableAutoSelect(ctx context.Context, _ A.Router, logger log.ContextLogger, tag string, options option.MutableAutoSelectOutboundOptions) (A.Outbound, error) {
	if p := options.SwitchTolerancePercent; p != nil && *p > 100 {
		return nil, fmt.Errorf("switch_tolerance_percent %d exceeds 100", *p)
	}
	cfg, hp := resolveMutableAutoSelectOptions(options)
	ctx, cancel := context.WithCancel(ctx)
	out := &MutableAutoSelect{
		Adapter:      outbound.NewAdapter(constant.TypeMutableAutoSelect, tag, []string{"tcp", "udp"}, nil),
		ctx:          ctx,
		cancel:       cancel,
		outboundMgr:  service.FromContext[A.OutboundManager](ctx),
		connMgr:      service.FromContext[A.ConnectionManager](ctx),
		logger:       logger,
		tags:         append([]string(nil), options.Outbounds...),
		urlOverrides: maps.Clone(options.URLOverrides),
		defaultURL:   options.URL,
		histories:    make(map[string]*localHistory),
		cfg:          cfg,
		hist:         hp,
		history:      resolveHistoryStorage(ctx),
		exhaustionCh: make(chan struct{}, 1),
	}
	if slogger, ok := logger.(lLog.SLogger); ok {
		out.logger = slogger.WithAttrs(slog.String("mutableautoselect_group", tag))
	}
	out.initProbeState()
	out.registerProbePauseCallback()
	return out, nil
}

func resolveHistoryStorage(ctx context.Context) adapter.AutoSelectHistoryStorage {
	if h := service.FromContext[adapter.AutoSelectHistoryStorage](ctx); h != nil {
		return h
	}
	return adapter.NewAutoSelectHistoryStorage()
}

// Start resolves every configured tag to an outbound atomically: a missing
// tag aborts without partially populating s.members.
func (s *MutableAutoSelect) Start() error {
	err := s.start()
	if err != nil {
		// sing-box doesn't Close an outbound whose start fails at runtime.
		s.unregisterProbePauseCallback()
	}
	return err
}

func (s *MutableAutoSelect) start() error {
	s.access.Lock()
	defer s.access.Unlock()

	if len(s.tags) == 0 {
		return nil
	}

	loaded := make(map[string]A.Outbound, len(s.tags))
	for _, tag := range s.tags {
		o, found := s.outboundMgr.Outbound(tag)
		if !found {
			return fmt.Errorf("outbound %s not found", tag)
		}
		loaded[tag] = o
	}
	for tag, o := range loaded {
		s.addMemberLocked(tag, o)
	}
	return nil
}

// addMemberLocked makes out tag's member, with its history restored and its
// probe state ready.
func (s *MutableAutoSelect) addMemberLocked(tag string, out A.Outbound) {
	s.members.Store(tag, out)
	s.hydrateHistoryLocked(tag)
	s.addProbeMemberLocked(tag)
}

// Caller must hold s.access. No-op when an in-memory entry already exists
// or when the persisted snapshot is older than cfg.maxPersistedAge.
func (s *MutableAutoSelect) hydrateHistoryLocked(tag string) {
	if _, ok := s.histories[tag]; ok {
		return
	}
	if s.history == nil {
		return
	}
	snap := s.history.Load(tag)
	if snap == nil {
		return
	}
	now := time.Now()
	if !snap.UpdatedAt.IsZero() && now.Sub(snap.UpdatedAt) > s.cfg.maxPersistedAge {
		// Stale snapshot from a prior session — typically describes a
		// candidate the bandit has rotated away from. Drop it from the
		// store too so a later All() doesn't surface it.
		s.history.Delete(tag)
		return
	}
	s.histories[tag] = hydrateLocalHistory(snap, now, s.hist.userFailureWindow)
}

// PostStart is idempotent. Background probing starts on the first dial or
// listen.
func (s *MutableAutoSelect) PostStart() error {
	s.access.Lock()
	if s.started {
		s.access.Unlock()
		return nil
	}
	s.started = true
	s.lastActive = time.Now()
	s.access.Unlock()
	s.CheckOutbounds()
	return nil
}

func (s *MutableAutoSelect) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.access.Lock()
		s.closeProbesLocked()
		close(s.exhaustionCh)
		s.access.Unlock()
		s.unregisterProbePauseCallback()
	})
	return nil
}

// InterfaceUpdated invalidates ordinary probes and requests replacements while probing is active.
func (s *MutableAutoSelect) InterfaceUpdated() {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return
	}
	s.advanceNetworkLocked()
	s.logger.Debug("interface updated: generation=", s.networkGeneration.Load())
	s.dispatchProbesLocked()
}

// paused reports whether the device or network is paused.
func (s *MutableAutoSelect) paused() bool {
	pm := service.FromContext[pause.Manager](s.ctx)
	return pm != nil && (pm.IsDevicePaused() || pm.IsNetworkPaused())
}

// Now reports the most recent dial-time selection, TCP preferred, UDP
// fallback. Single representative tag for telemetry; not a per-network
// route — TCP and UDP select independently at dial time.
func (s *MutableAutoSelect) Now() string {
	if t := loadString(&s.stickyTag.tcp); t != "" {
		return t
	}
	return loadString(&s.stickyTag.udp)
}

func (s *MutableAutoSelect) All() []string {
	s.access.Lock()
	defer s.access.Unlock()
	out := make([]string, len(s.tags))
	copy(out, s.tags)
	return out
}

func (s *MutableAutoSelect) Add(tags ...string) (n int, err error) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return 0, adapter.ErrGroupClosed
	}
	var missing, added []string
	for _, tag := range tags {
		if _, exists := s.members.Load(tag); exists {
			continue
		}
		// A tag may be in s.tags without a members entry after a partial
		// init failure. Don't re-append in that case.
		alreadyListed := slices.Contains(s.tags, tag)
		o, found := s.outboundMgr.Outbound(tag)
		if !found {
			missing = append(missing, tag)
			continue
		}
		s.addMemberLocked(tag, o)
		if !alreadyListed {
			s.tags = append(s.tags, tag)
		}
		added = append(added, tag)
		n++
	}
	if len(added) > 0 {
		s.requestProbesLocked(added, probeIfStale, false)
		s.notifyProbesLocked()
		s.dispatchProbesLocked()
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("%d outbounds not found: %v", len(missing), missing)
	}
	return n, nil
}

func (s *MutableAutoSelect) Remove(tags ...string) (n int, err error) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return 0, adapter.ErrGroupClosed
	}
	removed := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		out, exists := s.members.Load(tag)
		if !exists {
			continue
		}
		if attempt := s.inFlightProbes[out]; attempt != nil && attempt.tag == tag {
			s.cancelProbeLocked(attempt)
		}
		s.members.Delete(tag)
		delete(s.probeMembers, tag)
		delete(s.urlOverrides, tag)
		delete(s.histories, tag)
		if s.history != nil {
			s.history.Delete(tag)
		}
		s.clearStickyTagLocked(tag)
		removed[tag] = struct{}{}
		n++
	}
	if n > 0 {
		// Preserve registration order; rankLocked() ties on first-seen in s.tags.
		filtered := s.tags[:0]
		for _, tag := range s.tags {
			if _, gone := removed[tag]; gone {
				continue
			}
			filtered = append(filtered, tag)
		}
		s.tags = filtered
		s.notifyProbesLocked()
		s.dispatchProbesLocked()
	}
	return
}

// SetURLOverrides merges overrides into the per-member callback URL overrides.
// A member whose URL changes keeps its history but has its probe result
// marked stale.
func (s *MutableAutoSelect) SetURLOverrides(overrides map[string]string) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.urlOverrides == nil {
		s.urlOverrides = make(map[string]string, len(overrides))
	}
	for tag, u := range overrides {
		if s.urlOverrides[tag] == u {
			continue
		}
		s.urlOverrides[tag] = u
		s.requestProbesLocked([]string{tag}, probeForced, false)
	}
	s.notifyProbesLocked()
	s.dispatchProbesLocked()
}

// ExhaustionSignal returns a receive-only channel that emits when every
// candidate fails a ladder or background probe cycle. Buffers one pending
// signal; an unread signal is replaced by the next emission. Closed on
// group Close so range-loop consumers terminate cleanly.
func (s *MutableAutoSelect) ExhaustionSignal() <-chan struct{} {
	return s.exhaustionCh
}

func (s *MutableAutoSelect) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableAutoSelect.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", s.Network()),
		tagAttr("outbound", s.Now()),
		tagAttr("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	o, generation, err := s.selectOutbound(network, "")
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	outerTag := o.Tag()
	conn, err := o.DialContext(ctx, network, destination)
	if err == nil {
		return s.wrapStream(conn, network, o, primaryRoute, generation), nil
	}
	s.logger.ErrorContext(ctx, err)
	// Attribute the failure to the outer (member) tag so rankLocked sees
	// it. For nested groups, the inner tag isn't a member of this group,
	// so recordUserFailure would drop on the member gate.
	s.recordUserFailure(outerTag, adapter.UserFailureDial, generation)

	// Fast failover: try one alternate from the current rank. The active
	// 3-min cadence keeps the probe history fresh enough to pick a working
	// peer without re-probing. The network may have changed during the
	// primary dial, so the alternate carries its own generation.
	ladderGeneration := generation
	alt, altGeneration, altErr := s.selectOutbound(network, outerTag)
	if altErr == nil {
		replacementTag := alt.Tag()
		conn, err = alt.DialContext(ctx, network, destination)
		if err == nil {
			if network == N.NetworkTCP {
				s.requestFailureConfirmation(outerTag, replacementTag, generation)
			}
			return s.wrapStream(conn, network, alt, fallbackRoute, altGeneration), nil
		}
		s.logger.ErrorContext(ctx, err)
		s.recordUserFailure(replacementTag, adapter.UserFailureDial, altGeneration)
		outerTag, ladderGeneration = replacementTag, altGeneration
	}

	go s.runLadder(outerTag, ladderGeneration)
	recordError(span, err)
	return nil, err
}

func (s *MutableAutoSelect) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableAutoSelect.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		tagAttr("outbound", s.Now()),
		tagAttr("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	o, generation, err := s.selectOutbound("udp", "")
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	outerTag := o.Tag()
	conn, err := o.ListenPacket(ctx, destination)
	if err == nil {
		return s.wrapPacket(conn, o, primaryRoute, generation), nil
	}
	s.logger.ErrorContext(ctx, err)
	s.recordUserFailure(outerTag, adapter.UserFailureDial, generation)

	ladderGeneration := generation
	alt, altGeneration, altErr := s.selectOutbound("udp", outerTag)
	if altErr == nil {
		replacementTag := alt.Tag()
		conn, err = alt.ListenPacket(ctx, destination)
		if err == nil {
			return s.wrapPacket(conn, alt, fallbackRoute, altGeneration), nil
		}
		s.logger.ErrorContext(ctx, err)
		s.recordUserFailure(replacementTag, adapter.UserFailureDial, altGeneration)
		outerTag, ladderGeneration = replacementTag, altGeneration
	}

	go s.runLadder(outerTag, ladderGeneration)
	recordError(span, err)
	return nil, err
}

// routeKind records whether a conn used the selected member or a
// fast-failover alternate.
type routeKind uint8

const (
	primaryRoute routeKind = iota
	fallbackRoute
)

// chargeable reports whether a data-plane failure should count against tag.
// Primary-route failures are counted only while tag is selected for TCP or
// UDP; fallback-route failures are always counted as fresh failover traffic.
func (s *MutableAutoSelect) chargeable(tag string, route routeKind) bool {
	return !s.cfg.demoteOnlySelected || route == fallbackRoute ||
		loadString(&s.stickyTag.tcp) == tag || loadString(&s.stickyTag.udp) == tag
}

// wrapStream enables no-response detection for TCP only: on UDP a lost
// datagram or a destination that never replies looks the same as a dead
// member.
func (s *MutableAutoSelect) wrapStream(conn net.Conn, network string, o A.Outbound, route routeKind, generation uint64) net.Conn {
	hooks := s.makeHooks(o.Tag(), N.NetworkName(network), route, generation)
	wrapped := newDataPlaneStream(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	if N.NetworkName(network) == N.NetworkTCP {
		wrapped.firstResponseTimeout = s.cfg.dataPlaneFirstResponseTimeout
	}
	return adapter.NewTaggedConn(wrapped, realTag(o))
}

func (s *MutableAutoSelect) wrapPacket(conn net.PacketConn, o A.Outbound, route routeKind, generation uint64) net.PacketConn {
	hooks := s.makeHooks(o.Tag(), N.NetworkUDP, route, generation)
	wrapped := newDataPlanePacket(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	return adapter.NewTaggedPacketConn(wrapped, realTag(o))
}

func (s *MutableAutoSelect) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewPacketConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) selectOutbound(network, excludeTag string) (A.Outbound, uint64, error) {
	var slot *atomic.Value
	switch network {
	case "tcp":
		slot = &s.stickyTag.tcp
	case "udp":
		slot = &s.stickyTag.udp
	default:
		return nil, 0, fmt.Errorf("network %s not supported", network)
	}
	s.access.Lock()
	if excludeTag == "" {
		s.keepAliveLocked()
	}
	ranked := s.rankLocked(time.Now())
	generation := s.networkGeneration.Load()
	if excludeTag != "" {
		// A regular member still clean or soft-demoted, the excluded one
		// included, means the network isn't down to its last resort: one
		// failed dial shouldn't send a request through it, in any tier.
		regularHealthy := slices.ContainsFunc(ranked, func(c rankedCandidate) bool {
			return c.demote < demoteLastResort && slices.Contains(c.outbound.Network(), network)
		})
		ranked = slices.DeleteFunc(ranked, func(c rankedCandidate) bool {
			return c.tag == excludeTag || (regularHealthy && c.lastResort)
		})
	}
	pool, forNetwork := s.splitHealthyForLocked(ranked, network)
	if len(pool) == 0 {
		if excludeTag == "" {
			// No-pool on the primary path: probe so the next attempt has data.
			s.requestProbesLocked(nil, probeIfStale, false)
			s.dispatchProbesLocked()
		}
		s.access.Unlock()
		return nil, generation, errors.New("no outbound available, recovery in progress")
	}
	var winner rankedCandidate
	if excludeTag == "" {
		winner = s.applyStickiness(network, slot, pool[0], forNetwork)
		prev := loadString(slot)
		if prev != winner.tag {
			slot.Store(winner.tag)
			if prev == "" {
				s.logger.Info(network, " select: ", winner.tag)
			}
		}
	} else {
		winner = pool[0]
	}
	s.access.Unlock()
	return winner.outbound, generation, nil
}

// applyStickiness applies switch-tolerance hysteresis between the sticky tag
// and best, the pool's preferred candidate. It looks for the sticky tag
// across every demotion tier, not just the pool, so soft demotion still goes
// through the normal comparison instead of looking like removal.
//
// Retention is capped at softFailLimit failures; beyond that, continuing
// failures release the sticky even if it remains much faster.
//
// Caller must hold s.access.
func (s *MutableAutoSelect) applyStickiness(
	network string,
	slot *atomic.Value,
	best rankedCandidate,
	forNetwork []rankedCandidate,
) rankedCandidate {
	sticky := loadString(slot)
	if sticky == "" || sticky == best.tag {
		return best
	}
	idx := slices.IndexFunc(forNetwork, func(c rankedCandidate) bool {
		return c.tag == sticky
	})
	if idx < 0 {
		s.logSwitch(network, sticky, best, "not a candidate")
		return best
	}
	c := forNetwork[idx]
	switch {
	case (c.delayMs == 0) != (best.delayMs == 0):
		// The tolerance comparison only means something between two real
		// measurements: delayMs 0 means never measured, and comparing it with
		// a real delay would let a never-probed sticky's 0ms retain it.
		// rankLocked already sorts measured members first, so defer to it.
		s.logSwitch(network, sticky, best, "unmeasured")
	case c.demote == demoteHard && best.demote < demoteHard:
		s.logSwitch(network, sticky, best, "hard-demoted")
	case c.lastResort && !best.lastResort && best.demote <= c.demote:
		s.logSwitch(network, sticky, best, "last resort no longer needed")
	case c.demote > best.demote && c.userFails > s.hist.softFailLimit:
		s.logSwitch(network, sticky, best, "failures past retention")
	case s.beatsOnDelay(best.delayMs, c.delayMs):
		s.logSwitch(network, sticky, best, "beaten on delay")
	default:
		return c
	}
	return best
}

// beatsOnDelay reports whether candidateMs improves on stickyMs by at least
// switchTolerance and by at least switchTolerancePercent of stickyMs.
func (s *MutableAutoSelect) beatsOnDelay(candidateMs, stickyMs uint32) bool {
	if candidateMs >= stickyMs {
		return false
	}
	gain := uint64(stickyMs - candidateMs)
	return gain >= uint64(s.cfg.switchTolerance/time.Millisecond) &&
		gain*100 >= uint64(stickyMs)*uint64(s.cfg.switchTolerancePercent)
}

// logSwitch records which rule moved the group off sticky; a churn report is
// only actionable if the logs say that.
func (s *MutableAutoSelect) logSwitch(network, sticky string, best rankedCandidate, reason string) {
	s.logger.Info(network, " switch: ", sticky, " -> ", best.tag, " (", reason, ")")
}

// clearStickyTagLocked drops the sticky tag for any network where it
// equals tag. Caller must hold s.access.
func (s *MutableAutoSelect) clearStickyTagLocked(tag string) {
	for _, slot := range [...]*atomic.Value{&s.stickyTag.tcp, &s.stickyTag.udp} {
		if loadString(slot) == tag {
			slot.Store("")
		}
	}
}

func loadString(v *atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

func (s *MutableAutoSelect) probeURLForLocked(tag string) string {
	if u, ok := s.urlOverrides[tag]; ok && u != "" {
		return u
	}
	return s.defaultURL
}

// historyForLocked returns the history for tag, creating an empty one if
// none exists. Use only on the write path; read-only callers must use
// peekHistoryLocked so ranking a never-probed tag doesn't litter the map.
// Caller must hold s.access.
func (s *MutableAutoSelect) historyForLocked(tag string) *localHistory {
	h, ok := s.histories[tag]
	if !ok {
		h = newLocalHistory()
		s.histories[tag] = h
	}
	return h
}

func (s *MutableAutoSelect) peekHistoryLocked(tag string) (*localHistory, bool) {
	h, ok := s.histories[tag]
	return h, ok
}

// demoteLevel ranks how cautious selection should be about a candidate.
// Ordering is load-bearing: rankLocked sorts on the integer value, so
// demoteClean must compare < demoteSoft < demoteLastResort < demoteHard for
// the tiers to land in the right order.
type demoteLevel uint8

const (
	demoteClean      demoteLevel = iota
	demoteSoft                   // window_count(userFailures) >= 1, hard threshold not reached
	demoteLastResort             // a lastResort member that isn't hard-demoted
	demoteHard                   // consecutive_failures or windowed user-failures at limit
)

type rankedCandidate struct {
	outbound        A.Outbound
	tag             string
	delayMs         uint32
	demote          demoteLevel
	userFails       uint32
	lastResort      bool
	hasRecoveryGate bool
}

// splitHealthyForLocked filters ranked to network and prefers ungated members
// before choosing the cleanest non-empty tier (clean, soft, last resort, hard).
// Gated members remain eligible only when no ungated member is available, or
// when every ungated member is hard-demoted and a gated one is not, so the gate
// never forces selection onto a member already known to be failing.
// forNetwork retains every eligible tier for stickiness.
//
// ranked must be sorted by demote level, as rankLocked returns it. Requires
// s.access; both returned slices alias s.scratchSplit.
func (s *MutableAutoSelect) splitHealthyForLocked(ranked []rankedCandidate, network string) (pool, forNetwork []rankedCandidate) {
	clear(s.scratchSplit)
	out := s.scratchSplit[:0]
	for _, c := range ranked {
		if !slices.Contains(c.outbound.Network(), network) {
			continue
		}
		out = append(out, c)
	}
	var ungated, ungatedUsable, gatedUsable bool
	for _, c := range out {
		usable := c.demote < demoteHard
		if c.hasRecoveryGate {
			gatedUsable = gatedUsable || usable
		} else {
			ungated = true
			ungatedUsable = ungatedUsable || usable
		}
	}
	if ungatedUsable || (ungated && !gatedUsable) {
		out = slices.DeleteFunc(out, func(c rankedCandidate) bool { return c.hasRecoveryGate })
	}
	var nClean, nSoft, nLast int
	for _, c := range out {
		switch c.demote {
		case demoteClean:
			nClean++
		case demoteSoft:
			nSoft++
		case demoteLastResort:
			nLast++
		}
	}
	s.scratchSplit = out
	switch {
	case nClean > 0:
		return out[:nClean], out
	case nSoft > 0:
		return out[nClean : nClean+nSoft], out
	case nLast > 0:
		return out[nClean+nSoft : nClean+nSoft+nLast], out
	default:
		return out, out
	}
}

// preCandidate holds a member's pre-demotion state while rankLocked assembles
// the candidate set.
type preCandidate struct {
	o               A.Outbound
	tag             string
	delayMs         uint32
	consec          uint32
	userFails       uint32
	lastResort      bool
	hasRecoveryGate bool
}

// rankLocked requires s.access to be held.
func (s *MutableAutoSelect) rankLocked(now time.Time) []rankedCandidate {
	clear(s.scratchPres)
	pres := s.scratchPres[:0]
	for _, tag := range s.tags {
		o, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(o.Type())
		if beh.excludeFromPool {
			continue
		}
		var (
			rawDelay        uint32
			consec          uint32
			userFails       uint32
			hasRecoveryGate bool
		)
		if h, ok := s.peekHistoryLocked(tag); ok {
			lastDelay, c, uf, gated := h.snapshot(now, s.hist.userFailureWindow)
			rawDelay = lastDelay
			consec = c
			userFails = uint32(len(uf))
			hasRecoveryGate = gated
		} else if s.history != nil {
			if seed := s.history.Load(tag); seed != nil {
				if seed.UpdatedAt.IsZero() || now.Sub(seed.UpdatedAt) <= s.cfg.maxPersistedAge {
					rawDelay = seed.LastSuccessDelayMs
					consec = seed.ConsecutiveFailures
					userFails = countUserFailuresInWindow(seed.UserFailures, now, s.hist.userFailureWindow)
				}
			}
		}
		pres = append(pres, preCandidate{o: o, tag: tag, delayMs: rawDelay, consec: consec, userFails: userFails, lastResort: beh.lastResort, hasRecoveryGate: hasRecoveryGate})
	}
	s.scratchPres = pres

	// Switch-penalty awareness: when deciding whether to hard-demote a
	// candidate, demoted() needs to see the best alternative's delay so
	// the rule can soften when the only fallback is much slower. A 0 delay
	// means never measured, so it doesn't participate. lastResort members
	// don't either: they are not an alternative a regular member competes
	// with.
	var min1, min2 uint32
	for _, p := range pres {
		if p.delayMs == 0 || p.lastResort {
			continue
		}
		switch {
		case min1 == 0 || p.delayMs < min1:
			min2 = min1
			min1 = p.delayMs
		case min2 == 0 || p.delayMs < min2:
			min2 = p.delayMs
		}
	}

	clear(s.scratchRanked)
	out := s.scratchRanked[:0]
	for _, p := range pres {
		// selfMs stays 0 for an unmeasured candidate, which demoted reads as
		// no boost.
		var selfMs, bestAlt uint32
		// A last resort isn't rescued by the switch-penalty boost: the normal
		// hard threshold always applies to it.
		if p.delayMs > 0 && !p.lastResort {
			selfMs = p.delayMs
			bestAlt = min1
			if p.delayMs == min1 {
				bestAlt = min2
			}
		}
		hard, soft, _ := demoted(p.consec, p.userFails, selfMs, bestAlt, s.hist)
		level := demoteClean
		switch {
		case hard || s.stuckProbeLocked(p.o):
			level = demoteHard
		case p.lastResort:
			level = demoteLastResort
		case soft:
			level = demoteSoft
		}
		out = append(out, rankedCandidate{outbound: p.o, tag: p.tag, delayMs: p.delayMs, demote: level, userFails: p.userFails, lastResort: p.lastResort, hasRecoveryGate: p.hasRecoveryGate})
	}
	s.scratchRanked = out
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.demote != b.demote {
			return a.demote < b.demote
		}
		// Within a tier (in practice demoteHard, the only one both share), a
		// regular member outranks a last resort whatever their delays.
		if a.lastResort != b.lastResort {
			return !a.lastResort
		}
		// An unmeasured member (delay 0) ranks after measured ones.
		if (a.delayMs == 0) != (b.delayMs == 0) {
			return a.delayMs != 0
		}
		return a.delayMs < b.delayMs
	})
	return out
}

const lastResortSuccessFreshness = 5 * time.Minute

var probeBound = func(beh protocolBehavior) time.Duration {
	return beh.probeTimeout + 5*time.Second
}

// mutateHistory accepts mutations only for the current network generation.
func (s *MutableAutoSelect) mutateHistory(tag string, generation uint64, fn func(*localHistory, time.Time) bool) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if _, member := s.members.Load(tag); !member {
		return false
	}
	if !s.matchesGenerationLocked(generation) {
		return false
	}
	return s.mutateHistoryLocked(tag, fn)
}

// mutateHistoryLocked applies fn to tag's history and persists the result
// when fn reports a change.
func (s *MutableAutoSelect) mutateHistoryLocked(tag string, fn func(*localHistory, time.Time) bool) bool {
	h := s.historyForLocked(tag)
	now := time.Now()
	if !fn(h, now) {
		return false
	}
	if s.history != nil {
		s.history.Store(tag, h.toTagHistory(now, s.hist))
	}
	return true
}

// recordUserFailure reports whether a current member's failure was added to its window.
func (s *MutableAutoSelect) recordUserFailure(tag string, kind adapter.UserFailureKind, generation uint64) bool {
	recorded := s.mutateHistory(tag, generation, func(h *localHistory, now time.Time) bool {
		return h.addUserFailure(adapter.UserFailure{At: now, Kind: kind}, s.hist.userFailureWindow, s.hist.userFailureDedupeWindow)
	})
	if recorded {
		s.logger.Info("user failure: tag=", tag, " kind=", kind)
	}
	return recorded
}

func (s *MutableAutoSelect) trySwitchAfterConfirmedFailureLocked(failedTag, replacementTag string) {
	slot := &s.stickyTag.tcp
	if loadString(slot) != failedTag {
		return
	}
	if _, member := s.members.Load(failedTag); !member {
		return
	}
	h, ok := s.peekHistoryLocked(failedTag)
	if !ok {
		return
	}
	now := time.Now()
	if _, consec, _, _ := h.snapshot(now, s.hist.userFailureWindow); consec == 0 {
		return
	}
	if replacementTag == "" || !s.eligibleReplacementLocked(now, replacementTag) {
		replacementTag = s.findProbedReplacementLocked(now, N.NetworkTCP, failedTag)
	}
	if replacementTag == "" {
		return
	}
	last := s.lastConfirmedFailureSwitch
	if !last.at.IsZero() && now.Sub(last.at) < s.cfg.confirmedFailureSwitchCooldown && last.selectedTag != failedTag {
		s.logger.Info("tcp switch deferred: ", failedTag, " -> ", replacementTag, " (confirmed-failure switch cooldown)")
		return
	}
	slot.Store(replacementTag)
	h.startRecoveryGate(now)
	s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = now, replacementTag
	s.logger.Info("tcp switch: ", failedTag, " -> ", replacementTag, " (failed confirmation)")
}

// eligibleReplacementLocked requires membership, no recovery gate, and a
// passing latest probe, regardless of age. Caller must hold s.access.
func (s *MutableAutoSelect) eligibleReplacementLocked(now time.Time, tag string) bool {
	if out, member := s.members.Load(tag); !member || s.stuckProbeLocked(out) {
		return false
	}
	h, ok := s.peekHistoryLocked(tag)
	if !ok {
		return false
	}
	delay, consec, _, gated := h.snapshot(now, s.hist.userFailureWindow)
	return delay > 0 && consec == 0 && !gated
}

// findProbedReplacementLocked returns the highest-ranked member whose latest
// probe passed and that has no recovery gate, in network's healthiest tier
// after excluding excludeTag, or "" if there is none. Caller must hold s.access.
func (s *MutableAutoSelect) findProbedReplacementLocked(now time.Time, network, excludeTag string) string {
	ranked := slices.DeleteFunc(s.rankLocked(now), func(c rankedCandidate) bool {
		return c.tag == excludeTag
	})
	pool, _ := s.splitHealthyForLocked(ranked, network)
	for _, c := range pool {
		if s.eligibleReplacementLocked(now, c.tag) {
			return c.tag
		}
	}
	return ""
}

// emitExhaustionLocked requires s.access, which Close holds while closing the
// channel, so the send can't hit a closed channel.
func (s *MutableAutoSelect) emitExhaustionLocked() {
	if s.isClosed() {
		return
	}
	// Drop any unread pending signal; the channel buffers exactly one.
	select {
	case <-s.exhaustionCh:
	default:
	}
	select {
	case s.exhaustionCh <- struct{}{}:
	default:
	}
}

// noteDataActivity records data-plane traffic, which keeps background probing
// running but does not restart it.
func (s *MutableAutoSelect) noteDataActivity(now time.Time) {
	// The idle check needs only coarse precision, so skip most writes to a
	// field every connection's Read and Write would otherwise contend on.
	if n := now.UnixNano(); n-s.lastDataAt.Load() >= int64(time.Second) {
		s.lastDataAt.Store(n)
	}
}

func (s *MutableAutoSelect) isClosed() bool {
	select {
	case <-s.ctx.Done():
		return true
	default:
		return false
	}
}
