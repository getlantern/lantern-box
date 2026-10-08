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

const defaultProbeConcurrency = 6

// probeFreshnessWindow lets external probes skip members with recent outcomes.
// Internal probes always force a fresh probe.
const probeFreshnessWindow = 30 * time.Second

// confirmedFailureSwitchCooldown limits switches after confirmed failures,
// except when the member selected by the last such switch also fails confirmation.
const confirmedFailureSwitchCooldown = time.Minute

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

	// probeMu serializes batch probes. Targeted failure confirmations run
	// independently. Fire-and-forget batches TryLock; other batches Lock.
	probeMu   sync.Mutex
	laddering atomic.Bool
	// Unix-nano of the most recent runLadder completion. Read by the
	// cooldown gate to suppress back-to-back full-fleet re-probes when
	// stalls or dial errors arrive in quick succession.
	lastLadderAt atomic.Int64
	exhaustionCh chan struct{}

	// pendingFailureConfirmations holds tags with a targeted confirmation
	// probe in flight.
	pendingFailureConfirmations isync.TypedMap[string, struct{}]
	// Guarded by access.
	lastConfirmedFailureSwitch struct {
		at          time.Time
		selectedTag string
	}

	// externalProbeMu drops overlapping Add / CheckOutbounds probes; probeMu
	// also makes them drop during an internal cycle.
	externalProbeMu sync.Mutex

	// lastResortInFlight holds the lastResort members with a probe running,
	// so each has at most one at a time. Guarded by access.
	lastResortInFlight map[string]bool

	// Unix-nano of the most recent non-empty data-plane Read/Write; 0
	// means no traffic observed yet. Drives the adaptive probe cadence.
	lastActive atomic.Int64

	started   bool
	closeOnce sync.Once
}

type mutableAutoSelectConfig struct {
	switchTolerance               time.Duration
	activeInterval                time.Duration
	idleInterval                  time.Duration
	idleThreshold                 time.Duration
	ladderTotalBudget             time.Duration
	ladderCooldown                time.Duration
	dataPlaneIdle                 time.Duration
	dataPlaneFirstResponseTimeout time.Duration
	dataPlaneProvedRead           uint64
	demoteOnlySelected            bool
	maxPersistedAge               time.Duration
	probeConcurrency              int
}

func resolveMutableAutoSelectOptions(o option.MutableAutoSelectOutboundOptions) (mutableAutoSelectConfig, historyParams) {
	cfg := mutableAutoSelectConfig{
		switchTolerance:               time.Duration(o.SwitchToleranceMs) * time.Millisecond,
		activeInterval:                time.Duration(o.BackgroundIntervalSeconds) * time.Second,
		idleInterval:                  time.Duration(o.IdleIntervalSeconds) * time.Second,
		idleThreshold:                 time.Duration(o.IdleThresholdSeconds) * time.Second,
		ladderTotalBudget:             time.Duration(o.LadderTotalBudgetSeconds) * time.Second,
		ladderCooldown:                time.Duration(o.LadderCooldownSeconds) * time.Second,
		dataPlaneIdle:                 time.Duration(o.DataPlaneIdleSeconds) * time.Second,
		dataPlaneFirstResponseTimeout: defaultFirstResponseTimeout,
		dataPlaneProvedRead:           uint64(o.DataPlaneProvedReadBytes),
		demoteOnlySelected:            o.DemoteOnlySelectedTag == nil || *o.DemoteOnlySelectedTag,
		maxPersistedAge:               time.Duration(o.MaxPersistedAgeSeconds) * time.Second,
		probeConcurrency:              int(o.ProbeConcurrency),
	}
	if cfg.switchTolerance == 0 {
		cfg.switchTolerance = 200 * time.Millisecond
	}
	if cfg.activeInterval == 0 {
		cfg.activeInterval = 180 * time.Second
	}
	if cfg.idleInterval == 0 {
		cfg.idleInterval = 900 * time.Second
	}
	if cfg.idleThreshold == 0 {
		cfg.idleThreshold = 600 * time.Second
	}
	if cfg.ladderTotalBudget == 0 {
		cfg.ladderTotalBudget = 10 * time.Second
	}
	if cfg.ladderCooldown == 0 {
		cfg.ladderCooldown = 60 * time.Second
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
	return cfg, hp
}

func NewMutableAutoSelect(ctx context.Context, _ A.Router, logger log.ContextLogger, tag string, options option.MutableAutoSelectOutboundOptions) (A.Outbound, error) {
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
		nfact := lLog.NewFactory(slogger.SlogHandler().WithAttrs([]slog.Attr{slog.String("mutableautoselect_group", tag)}))
		out.logger = nfact.Logger()
	}
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
		s.members.Store(tag, o)
		s.hydrateHistoryLocked(tag)
	}
	return nil
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

// PostStart is idempotent: a second invocation is a no-op rather than a
// duplicate background loop.
func (s *MutableAutoSelect) PostStart() error {
	s.access.Lock()
	if s.started {
		s.access.Unlock()
		return nil
	}
	s.started = true
	s.access.Unlock()
	go s.runBackgroundLoop()
	s.CheckOutbounds()
	return nil
}

func (s *MutableAutoSelect) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		// Close under s.access so emitExhaustion (which holds the same
		// lock around its send) can't race a send into a closed channel.
		s.access.Lock()
		close(s.exhaustionCh)
		s.access.Unlock()
	})
	return nil
}

func (s *MutableAutoSelect) InterfaceUpdated() {
	s.logger.Info("interface updated, re-probing mutableautoselect group")
	go s.runProbeCycle(s.ctx)
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
		s.members.Store(tag, o)
		s.hydrateHistoryLocked(tag)
		if !alreadyListed {
			s.tags = append(s.tags, tag)
		}
		added = append(added, tag)
		n++
	}
	// Probe new members so dial-time ranking has data before the next
	// background cycle.
	if len(added) > 0 {
		go s.runExternalProbe(added)
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
		if _, exists := s.members.Load(tag); !exists {
			continue
		}
		s.members.Delete(tag)
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
	}
	return
}

// SetURLOverrides replaces per-member callback URL overrides.
// Changed or removed overrides keep history and mark probe results stale.
func (s *MutableAutoSelect) SetURLOverrides(overrides map[string]string) {
	s.access.Lock()
	defer s.access.Unlock()
	old := s.urlOverrides
	s.urlOverrides = maps.Clone(overrides)
	for tag, v := range s.urlOverrides {
		if old[tag] != v {
			s.markProbeStaleLocked(tag)
		}
	}
	for tag := range old {
		if _, kept := s.urlOverrides[tag]; !kept {
			s.markProbeStaleLocked(tag)
		}
	}
}

// markProbeStaleLocked also clears a persisted-only entry, which Start or Add
// would otherwise hydrate as a fresh outcome. Caller must hold s.access.
func (s *MutableAutoSelect) markProbeStaleLocked(tag string) {
	if h, ok := s.peekHistoryLocked(tag); ok {
		h.clearOutcomeAt()
		if s.history != nil {
			s.history.Store(tag, h.toTagHistory(time.Now(), s.hist))
		}
		return
	}
	if s.history == nil {
		return
	}
	// UpdatedAt is kept so the entry still ages out on its original schedule.
	if snap := s.history.Load(tag); snap != nil && !snap.LastOutcomeAt.IsZero() {
		snap.LastOutcomeAt = time.Time{}
		s.history.Store(tag, snap)
	}
}

func (s *MutableAutoSelect) CheckOutbounds() {
	go s.runExternalProbe(nil)
}

// ExhaustionSignal returns a receive-only channel that emits when every
// candidate fails within a ladder's budget. Buffers one pending signal;
// an unread signal is replaced by the next ladder's emission. Closed on
// group Close so range-loop consumers terminate cleanly.
func (s *MutableAutoSelect) ExhaustionSignal() <-chan struct{} {
	return s.exhaustionCh
}

// URLTest implements sing-box's URLTestGroup contract: probes every
// member in parallel and returns delay_ms per success. Used by the
// offline pre-warm path before Start, so members are resolved lazily
// from the outbound manager when not already loaded.
func (s *MutableAutoSelect) URLTest(ctx context.Context) (map[string]uint16, error) {
	results := make(map[string]uint16)

	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	s.access.Lock()
	for _, tag := range s.tags {
		if _, ok := s.members.Load(tag); ok {
			continue
		}
		o, found := s.outboundMgr.Outbound(tag)
		if !found {
			continue
		}
		s.members.Store(tag, o)
		// Hydrate before the first recordProbeOutcome lands so the
		// persisted scalars aren't clobbered by a one-entry write from
		// an empty in-memory localHistory.
		s.hydrateHistoryLocked(tag)
	}
	s.access.Unlock()

	s.internalProbe(ctx, func(res probeResult) {
		results[res.tag] = uint16(min(65535, res.delayMs))
	})
	return results, nil
}

func (s *MutableAutoSelect) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableAutoSelect.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	o, err := s.selectFor(network)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	outerTag := o.Tag()
	conn, err := o.DialContext(ctx, network, destination)
	if err == nil {
		return s.wrapStream(conn, network, o, primaryRoute), nil
	}
	s.logger.ErrorContext(ctx, err)
	// Attribute the failure to the outer (member) tag so rankLocked and
	// runLadder's gating both see it. For nested groups, the inner tag
	// isn't a member of this group, so recordUserFailure would drop on
	// the member gate.
	s.recordUserFailure(outerTag, adapter.UserFailureDial)

	// Fast failover: try one alternate from the current rank before
	// kicking the ladder. The active 3-min cadence keeps the probe
	// history fresh enough to pick a working peer without re-probing.
	alt, altErr := s.selectForExcluding(network, outerTag)
	if altErr == nil {
		replacementTag := alt.Tag()
		conn, err = alt.DialContext(ctx, network, destination)
		if err == nil {
			go s.runLadder(outerTag)
			if network == N.NetworkTCP {
				go s.confirmSelectedFailure(outerTag, replacementTag)
			}
			return s.wrapStream(conn, network, alt, fallbackRoute), nil
		}
		s.logger.ErrorContext(ctx, err)
		s.recordUserFailure(replacementTag, adapter.UserFailureDial)
		outerTag = replacementTag
	}

	go s.runLadder(outerTag)
	span.RecordError(err)
	return nil, err
}

func (s *MutableAutoSelect) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableAutoSelect.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	o, err := s.selectFor("udp")
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	outerTag := o.Tag()
	conn, err := o.ListenPacket(ctx, destination)
	if err == nil {
		return s.wrapPacket(conn, o, primaryRoute), nil
	}
	s.logger.ErrorContext(ctx, err)
	s.recordUserFailure(outerTag, adapter.UserFailureDial)

	alt, altErr := s.selectForExcluding("udp", outerTag)
	if altErr == nil {
		replacementTag := alt.Tag()
		conn, err = alt.ListenPacket(ctx, destination)
		if err == nil {
			go s.runLadder(outerTag)
			return s.wrapPacket(conn, alt, fallbackRoute), nil
		}
		s.logger.ErrorContext(ctx, err)
		s.recordUserFailure(replacementTag, adapter.UserFailureDial)
		outerTag = replacementTag
	}

	go s.runLadder(outerTag)
	span.RecordError(err)
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
func (s *MutableAutoSelect) wrapStream(conn net.Conn, network string, o A.Outbound, route routeKind) net.Conn {
	hooks := s.makeHooks(o.Tag(), N.NetworkName(network), route)
	wrapped := newDataPlaneStream(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	if N.NetworkName(network) == N.NetworkTCP {
		wrapped.firstResponseTimeout = s.cfg.dataPlaneFirstResponseTimeout
	}
	return adapter.NewTaggedConn(wrapped, realTag(o))
}

func (s *MutableAutoSelect) wrapPacket(conn net.PacketConn, o A.Outbound, route routeKind) net.PacketConn {
	hooks := s.makeHooks(o.Tag(), N.NetworkUDP, route)
	wrapped := newDataPlanePacket(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	return adapter.NewTaggedPacketConn(wrapped, realTag(o))
}

func (s *MutableAutoSelect) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewPacketConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) selectFor(network string) (A.Outbound, error) {
	return s.selectForExcluding(network, "")
}

// selectForExcluding ranks the current members for the requested network
// and returns the winner. excludeTag is dropped from the pool before
// ranking — used by the dial-site fast-failover path to skip the tag
// that just failed. An empty excludeTag is equivalent to plain selectFor.
//
// The sticky tag is only updated on the non-excluding path: a fast-
// failover pick shouldn't poison the next dial's hysteresis with a tag
// that was a fallback choice, not a steady-state winner.
func (s *MutableAutoSelect) selectForExcluding(network, excludeTag string) (A.Outbound, error) {
	var slot *atomic.Value
	switch network {
	case "tcp":
		slot = &s.stickyTag.tcp
	case "udp":
		slot = &s.stickyTag.udp
	default:
		return nil, fmt.Errorf("network %s not supported", network)
	}
	s.access.Lock()
	ranked := s.rankLocked(time.Now(), time.Time{})
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
		s.access.Unlock()
		if excludeTag == "" {
			// No-pool on the primary path: kick a fresh probe cycle so
			// the next attempt has data.
			s.CheckOutbounds()
		}
		return nil, errors.New("no outbound available, recovery in progress")
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
	return winner.outbound, nil
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
	case c.kind != best.kind:
		// The tolerance comparison only means something between measurements
		// of the same nature: kindUnknown carries delayMs 0 and
		// kindSubstituted a synthetic constant, so comparing across kinds
		// would let a never-probed sticky's 0ms retain it against a real
		// measurement. rankLocked already sorts demote, then kind, then
		// delay, so defer to that ordering.
		s.logSwitch(network, sticky, best, "kind outranked")
	case c.demote == demoteHard && best.demote < demoteHard:
		s.logSwitch(network, sticky, best, "hard-demoted")
	case c.lastResort && !best.lastResort && best.demote <= c.demote:
		s.logSwitch(network, sticky, best, "last resort no longer needed")
	case c.demote > best.demote && c.userFails > s.hist.softFailLimit:
		s.logSwitch(network, sticky, best, "failures past retention")
	case uint64(best.delayMs)+uint64(s.cfg.switchTolerance/time.Millisecond) <= uint64(c.delayMs):
		s.logSwitch(network, sticky, best, "beaten on delay")
	default:
		return c
	}
	return best
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

type probeJob struct {
	outbound A.Outbound
	probeURL string
	beh      protocolBehavior
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

// collectProbeJobsLocked builds jobs for tags, or all members when tags is nil.
// excludeFromPool and lastResort members are skipped; lastResort members are
// probed by kickLastResortProbesLocked instead. With force=false, members with
// outcomes newer than probeFreshnessWindow are skipped. Caller must hold
// s.access.
func (s *MutableAutoSelect) collectProbeJobsLocked(now time.Time, tags []string, force bool) []probeJob {
	if tags == nil {
		tags = s.tags
	}
	jobs := make([]probeJob, 0, len(tags))
	for _, tag := range tags {
		o, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(o.Type())
		if beh.excludeFromPool || beh.lastResort {
			continue
		}
		if !force {
			if h, ok := s.peekHistoryLocked(tag); ok {
				if at := h.outcomeAt(); !at.IsZero() && now.Sub(at) < probeFreshnessWindow {
					continue
				}
			}
		}
		jobs = append(jobs, probeJob{
			outbound: o,
			probeURL: s.probeURLForLocked(tag),
			beh:      beh,
		})
	}
	return jobs
}

// candidateKind classifies confidence in a candidate's delay measurement.
// Ordering is load-bearing: rankLocked's sort treats the integer value as the
// tie-break key, so real-seeded must come first and substituted last.
type candidateKind uint8

const (
	kindRealSeeded  candidateKind = iota // measured delay from a real probe
	kindUnknown                          // no in-memory data and no persisted seed
	kindSubstituted                      // protocol carries a substituteDelay (samizdat)
)

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
	kind            candidateKind
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
	kind            candidateKind
	consec          uint32
	userFails       uint32
	lastResort      bool
	hasRecoveryGate bool
}

// rankLocked builds the candidate set for selection. A non-zero freshSince
// restricts to members with a recorded outcome at or after freshSince that
// have a recorded success from any cycle. A fresh failure still qualifies,
// because recordProbeFailure preserves lastSuccessDelayMs, so a caller
// asking "did anything succeed this cycle?" must gate on that itself.
// Caller must hold s.access.
func (s *MutableAutoSelect) rankLocked(now time.Time, freshSince time.Time) []rankedCandidate {
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
			haveData        bool
		)
		if h, ok := s.peekHistoryLocked(tag); ok {
			lastDelay, lastAt, c, uf, gated := h.snapshot(now, s.hist.userFailureWindow)
			if !freshSince.IsZero() && lastAt.Before(freshSince) {
				continue
			}
			rawDelay = lastDelay
			consec = c
			userFails = uint32(len(uf))
			hasRecoveryGate = gated
			haveData = true
		} else if freshSince.IsZero() && s.history != nil {
			// Cold member (added without going through hydrate). Fall back
			// to the persisted snapshot so recompute-style callers see
			// host-injected data. Probe-cycle callers (freshSince set)
			// skip this branch — they only count outcomes from this cycle.
			if seed := s.history.Load(tag); seed != nil {
				if seed.UpdatedAt.IsZero() || now.Sub(seed.UpdatedAt) <= s.cfg.maxPersistedAge {
					rawDelay = seed.LastSuccessDelayMs
					consec = seed.ConsecutiveFailures
					userFails = countUserFailuresInWindow(seed.UserFailures, now, s.hist.userFailureWindow)
					haveData = true
				}
			}
		}
		if !freshSince.IsZero() && (!haveData || rawDelay == 0) {
			continue
		}
		var (
			delay uint32
			kind  candidateKind
		)
		switch {
		case beh.substituteDelay > 0:
			delay, kind = uint32(beh.substituteDelay/time.Millisecond), kindSubstituted
		case rawDelay == 0:
			delay, kind = 0, kindUnknown
		default:
			delay, kind = rawDelay, kindRealSeeded
		}
		pres = append(pres, preCandidate{o: o, tag: tag, delayMs: delay, kind: kind, consec: consec, userFails: userFails, lastResort: beh.lastResort, hasRecoveryGate: hasRecoveryGate})
	}
	s.scratchPres = pres

	// Switch-penalty awareness: when deciding whether to hard-demote a
	// candidate, demoted() needs to see the best alternative's delay so
	// the rule can soften when the only fallback is much slower. Only
	// kindRealSeeded delays participate — substituted/unknown values are
	// synthetic, so basing a "switching is costly" decision on them
	// would be meaningless. lastResort members don't participate either: they
	// are not an alternative a regular member competes with.
	var min1, min2 uint32
	for _, p := range pres {
		if p.kind != kindRealSeeded || p.delayMs == 0 || p.lastResort {
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
		// Pass selfMs=0 for non-real-seeded candidates so the boost
		// can't trigger off a synthetic delay. bestAlt for those is
		// irrelevant — demoted gates the boost on selfMs>0.
		var selfMs, bestAlt uint32
		// A last resort isn't rescued by the switch-penalty boost: the normal
		// hard threshold always applies to it.
		if p.kind == kindRealSeeded && !p.lastResort {
			selfMs = p.delayMs
			bestAlt = min1
			if p.delayMs == min1 {
				bestAlt = min2
			}
		}
		hard, soft, _ := demoted(p.consec, p.userFails, selfMs, bestAlt, s.hist)
		level := demoteClean
		switch {
		case hard:
			level = demoteHard
		case p.lastResort:
			level = demoteLastResort
		case soft:
			level = demoteSoft
		}
		out = append(out, rankedCandidate{outbound: p.o, tag: p.tag, delayMs: p.delayMs, kind: p.kind, demote: level, userFails: p.userFails, lastResort: p.lastResort, hasRecoveryGate: p.hasRecoveryGate})
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
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.delayMs < b.delayMs
	})
	return out
}

// runProbeCycle is the fire-and-forget internal probe path; it skips when
// another internal cycle is already in flight.
func (s *MutableAutoSelect) runProbeCycle(ctx context.Context) {
	if !s.probeMu.TryLock() {
		return
	}
	defer s.probeMu.Unlock()
	s.internalProbe(ctx, nil)
}

// runExternalProbe runs a fire-and-forget, freshness-filtered probe for tags,
// or all members when tags is nil. Its probe wave drops if another external
// probe or internal cycle is running (last-resort probes start regardless),
// and it does not rank after refreshing history.
func (s *MutableAutoSelect) runExternalProbe(tags []string) {
	// Last-resort probes run on their own goroutines and need only s.access,
	// so start them before the probe-wave locks: a wave already in flight
	// must not hold back the probe that reports a fresh callback URL.
	s.access.Lock()
	s.kickLastResortProbesLocked(time.Now(), tags, false)
	s.access.Unlock()

	if !s.externalProbeMu.TryLock() {
		return
	}
	defer s.externalProbeMu.Unlock()
	if !s.probeMu.TryLock() {
		return
	}
	defer s.probeMu.Unlock()

	s.access.Lock()
	jobs := s.collectProbeJobsLocked(time.Now(), tags, false)
	s.access.Unlock()

	s.probeAll(s.ctx, jobs, nil)
}

// internalProbe probes every non-excluded member, streaming successes to
// onSuccess when provided. Caller must hold s.probeMu. Unlike external probes,
// it bypasses freshness filtering; callers rank separately when needed.
func (s *MutableAutoSelect) internalProbe(ctx context.Context, onSuccess func(probeResult)) {
	s.access.Lock()
	now := time.Now()
	jobs := s.collectProbeJobsLocked(now, nil, true)
	s.kickLastResortProbesLocked(now, nil, true)
	s.access.Unlock()
	s.probeAll(ctx, jobs, onSuccess)
}

// kickLastResortProbesLocked starts a probe for each lastResort member in
// tags (all members when nil) that has none running. Each runs on its own
// goroutine under the group's lifetime with the member's long probeTimeout,
// so neither a probe wave nor URLTest waits for it; its outcome lands in
// history like any other. With force=false, members with an outcome newer
// than probeFreshnessWindow are skipped. Caller must hold s.access.
func (s *MutableAutoSelect) kickLastResortProbesLocked(now time.Time, tags []string, force bool) {
	if tags == nil {
		tags = s.tags
	}
	for _, tag := range tags {
		o, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(o.Type())
		if !beh.lastResort || s.lastResortInFlight[tag] {
			continue
		}
		if !force {
			if h, ok := s.peekHistoryLocked(tag); ok {
				if at := h.outcomeAt(); !at.IsZero() && now.Sub(at) < probeFreshnessWindow {
					continue
				}
			}
		}
		if s.lastResortInFlight == nil {
			s.lastResortInFlight = make(map[string]bool)
		}
		s.lastResortInFlight[tag] = true
		probeURL := s.probeURLForLocked(tag)
		bound := lastResortProbeBound(beh)
		go func() {
			done := make(chan probeResult, 1)
			go func() { done <- probeMember(s.ctx, o, probeURL, beh) }()
			watchdog := time.NewTimer(bound)
			defer watchdog.Stop()
			var res probeResult
			select {
			case res = <-done:
			case <-watchdog.C:
				// The outbound overran the probe deadline. Count the probe
				// as failed now so ranking sees it, but keep the slot until
				// the dial actually returns: freeing it would let each later
				// cycle stack another stuck dial on a stalled peer.
				if s.ctx.Err() == nil {
					s.recordLastResortOutcome(tag, probeURL, false, 0)
				}
				<-done
				s.finishLastResortProbe(tag, probeURL)
				return
			case <-s.ctx.Done():
			}
			// Record before freeing the slot, so a newer probe can't finish
			// first and have its outcome overwritten by this older one.
			// Group shutdown is not member evidence.
			if s.ctx.Err() == nil {
				s.recordLastResortOutcome(tag, probeURL, res.success, res.delayMs)
			}
			s.finishLastResortProbe(tag, probeURL)
		}()
	}
}

// recordLastResortOutcome records a last-resort probe's outcome unless the
// member's probe URL changed while it ran. A new URL (a new bandit callback
// token) marks the member's probe outcome stale, and an outcome for the old
// one would read as current.
func (s *MutableAutoSelect) recordLastResortOutcome(tag, probeURL string, success bool, delayMs uint32) {
	// Rechecked under s.access so a concurrent Close can't slip between the
	// caller's shutdown check and the write.
	guard := func() bool { return s.ctx.Err() == nil && s.probeURLForLocked(tag) == probeURL }
	s.mutateHistoryIf(tag, guard, func(h *localHistory, now time.Time) bool {
		if success {
			h.recordProbeSuccess(delayMs, now)
		} else {
			h.recordProbeFailure(now)
		}
		return true
	})
}

// finishLastResortProbe frees tag's probe slot. If the member's probe URL
// changed while the probe ran (a config update brought a new bandit callback
// URL), it starts a probe of the new URL at once rather than leaving it for
// the next background cycle.
func (s *MutableAutoSelect) finishLastResortProbe(tag, probeURL string) {
	s.access.Lock()
	defer s.access.Unlock()
	delete(s.lastResortInFlight, tag)
	if s.ctx.Err() == nil && s.probeURLForLocked(tag) != probeURL {
		s.kickLastResortProbesLocked(time.Now(), []string{tag}, true)
	}
}

// lastResortSuccessFreshness bounds how old a last resort's latest successful
// probe may be for the ladder to treat it as a winner.
const lastResortSuccessFreshness = 5 * time.Minute

// lastResortProbeBound is how long a last-resort probe may run before it is
// counted as failed even if the outbound hasn't returned. A variable so tests
// can shorten it.
var lastResortProbeBound = func(beh protocolBehavior) time.Duration {
	return beh.probeTimeout + 5*time.Second
}

// mutateHistory applies fn to tag's history under s.access and persists
// the result when fn returns true. Drops silently when tag is no longer a
// member. Holds the lock through both the in-memory mutation and the
// persistence write so a concurrent Remove can't be undone by a late
// goroutine. fn is passed the timestamp used for both the entry and the
// persisted snapshot. Returns true only if fn ran and reported a change.
func (s *MutableAutoSelect) mutateHistory(tag string, fn func(*localHistory, time.Time) bool) bool {
	return s.mutateHistoryIf(tag, nil, fn)
}

// mutateHistoryIf is mutateHistory gated on guard, which runs under s.access
// before any history entry is created; a nil guard always passes.
func (s *MutableAutoSelect) mutateHistoryIf(tag string, guard func() bool, fn func(*localHistory, time.Time) bool) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if _, member := s.members.Load(tag); !member {
		return false
	}
	if guard != nil && !guard() {
		return false
	}
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

// recordUserFailure appends a single user-traffic failure of the given
// kind to the member's sliding window and persists the snapshot. Dial
// errors and confirmed data-plane stalls both pass through here; each counts
// as one failure, with softFailLimit tripping the soft tier and
// consecutiveFailLimit tripping the hard tier. Returns true if the
// failure was persisted; false on dedup or non-member tag, so callers
// can suppress downstream side effects (e.g. ladder kicks) on collapsed
// events.
func (s *MutableAutoSelect) recordUserFailure(tag string, kind adapter.UserFailureKind) bool {
	recorded := s.mutateHistory(tag, func(h *localHistory, now time.Time) bool {
		return h.addUserFailure(adapter.UserFailure{At: now, Kind: kind}, s.hist.userFailureWindow, s.hist.userFailureDedupeWindow)
	})
	if recorded {
		s.logger.Info("user failure: tag=", tag, " kind=", kind)
	}
	return recorded
}

func (s *MutableAutoSelect) recordProbeOutcome(tag string, success bool, delayMs uint32) {
	s.mutateHistory(tag, func(h *localHistory, now time.Time) bool {
		if success {
			h.recordProbeSuccess(delayMs, now)
		} else {
			h.recordProbeFailure(now)
		}
		return true
	})
}

// confirmSelectedFailure coalesces probes of the selected TCP member after a
// user failure. A failed probe attempts a switch to replacementTag, or another
// eligible member if empty. Cancellation leaves selection unchanged.
func (s *MutableAutoSelect) confirmSelectedFailure(failedTag, replacementTag string) {
	if loadString(&s.stickyTag.tcp) != failedTag {
		return
	}
	if _, inFlight := s.pendingFailureConfirmations.LoadOrStore(failedTag, struct{}{}); inFlight {
		return
	}
	defer s.pendingFailureConfirmations.Delete(failedTag)

	o, member := s.members.Load(failedTag)
	if !member {
		return
	}
	beh := behaviorFor(o.Type())
	s.access.Lock()
	probeURL := s.probeURLForLocked(failedTag)
	s.access.Unlock()
	if beh.excludeFromPool || probeURL == "" {
		return
	}
	res := probeMember(s.ctx, o, probeURL, beh)
	if s.ctx.Err() != nil {
		return
	}
	s.recordProbeOutcome(failedTag, res.success, res.delayMs)
	if !res.success {
		s.trySwitchAfterConfirmedFailure(failedTag, replacementTag)
	}
}

// trySwitchAfterConfirmedFailure gates failedTag only if it is still selected,
// its latest probe still failed, and a switch to an eligible replacement
// succeeds. An ineligible replacementTag falls back to another eligible member.
func (s *MutableAutoSelect) trySwitchAfterConfirmedFailure(failedTag, replacementTag string) {
	s.access.Lock()
	defer s.access.Unlock()
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
	// A concurrent ladder probe may have recorded a newer success.
	if _, _, consec, _, _ := h.snapshot(now, s.hist.userFailureWindow); consec == 0 {
		return
	}
	if replacementTag == "" || !s.eligibleReplacementLocked(now, replacementTag) {
		replacementTag = s.findProbedReplacementLocked(now, N.NetworkTCP, failedTag)
	}
	if replacementTag == "" {
		return
	}
	last := s.lastConfirmedFailureSwitch
	if !last.at.IsZero() && now.Sub(last.at) < confirmedFailureSwitchCooldown && last.selectedTag != failedTag {
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
	if _, member := s.members.Load(tag); !member {
		return false
	}
	h, ok := s.peekHistoryLocked(tag)
	if !ok {
		return false
	}
	delay, _, consec, _, gated := h.snapshot(now, s.hist.userFailureWindow)
	return delay > 0 && consec == 0 && !gated
}

// findProbedReplacementLocked returns the highest-ranked member whose latest
// probe passed and that has no recovery gate, in network's healthiest tier
// after excluding excludeTag, or "" if there is none. Caller must hold s.access.
func (s *MutableAutoSelect) findProbedReplacementLocked(now time.Time, network, excludeTag string) string {
	ranked := slices.DeleteFunc(s.rankLocked(now, time.Time{}), func(c rankedCandidate) bool {
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

// runLadder is invoked on any dial/listen error (after the dial-site
// fast-failover step has already had its shot) and on data-plane stall
// callbacks. It does not retry the failing member: the dial caller
// already retries on its own schedule, and the fast-failover path has
// already exercised the next-best alternate. The ladder's job is to
// reshape the candidate pool: full parallel re-probe; if nothing
// succeeds inside ladderTotalBudget, emit the exhaustion signal so
// radiance can refetch /config-new.
//
// A ladderCooldown window after the previous run suppresses repeat
// kicks: probe data is still fresh, and rank demotion via
// recordUserFailure already moves traffic off bad peers without
// needing the ladder to re-shape the pool.
//
// target is for diagnostic logging only and to gate concurrent
// invocations through laddering's CAS.
func (s *MutableAutoSelect) runLadder(target string) {
	if s.isClosed() {
		return
	}
	if last := s.lastLadderAt.Load(); last != 0 {
		if time.Since(time.Unix(0, last)) < s.cfg.ladderCooldown {
			return
		}
	}
	if !s.laddering.CompareAndSwap(false, true) {
		return
	}
	defer s.laddering.Store(false)

	s.logger.Info("ladder: full re-probe (target=", target, ")")
	// Acquire probeMu before computing the budget so a slow background
	// cycle holding the mutex doesn't eat into the ladder's actual
	// probing time.
	s.probeMu.Lock()
	fullCtx, cancel := context.WithTimeout(s.ctx, s.cfg.ladderTotalBudget)
	defer cancel()
	cycleStart := time.Now()
	// A probe failure advances lastOutcomeAt while leaving
	// lastSuccessDelayMs intact, so the ranked set alone cannot tell a
	// member that succeeded this cycle from one that only failed in it.
	// Rank still orders the winner; this decides whether one exists. The
	// map needs no lock of its own: probeAll serializes onSuccess, and
	// internalProbe returns only after its workers finish.
	succeeded := make(map[string]struct{})
	s.internalProbe(fullCtx, func(res probeResult) {
		succeeded[res.tag] = struct{}{}
	})
	var winner A.Outbound
	s.access.Lock()
	for _, c := range s.rankLocked(time.Now(), cycleStart) {
		if _, ok := succeeded[c.tag]; ok {
			winner = c.outbound
			break
		}
	}
	if winner == nil {
		// lastResort members are probed asynchronously and can't report
		// inside the ladder budget. One whose latest probe succeeded within
		// lastResortSuccessFreshness can still carry traffic, so the group
		// isn't exhausted. An older success doesn't count: a later failure
		// doesn't rerun the ladder, so trusting it could suppress the
		// exhaustion signal for good.
		now := time.Now()
		for _, c := range s.rankLocked(now, time.Time{}) {
			if c.demote != demoteLastResort {
				continue
			}
			if h, ok := s.peekHistoryLocked(c.tag); ok {
				if delay, at, consec, _, _ := h.snapshot(now, s.hist.userFailureWindow); delay > 0 && consec == 0 && now.Sub(at) <= lastResortSuccessFreshness {
					winner = c.outbound
					break
				}
			}
		}
	}
	s.access.Unlock()
	s.probeMu.Unlock()
	// Stamp on completion, not entry, so a slow ladder's runtime counts
	// toward the cooldown.
	s.lastLadderAt.Store(time.Now().UnixNano())
	if s.isClosed() {
		return
	}
	if winner == nil {
		s.logger.Warn("ladder exhausted: no candidate succeeded")
		s.emitExhaustion()
		return
	}
	s.logger.Info("ladder found winner: ", winner.Tag())
}

func (s *MutableAutoSelect) emitExhaustion() {
	// Hold s.access across drain+send so Close (which closes the channel
	// under the same lock) can't turn the send into a panic-on-closed.
	s.access.Lock()
	defer s.access.Unlock()
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

func (s *MutableAutoSelect) bumpActive() {
	s.lastActive.Store(time.Now().UnixNano())
}

// nextProbeInterval picks the cadence for the next background probe. A
// brand-new group (lastActive == 0) is treated as idle so we don't burn
// the fast cadence on a tunnel nobody is using yet.
func (s *MutableAutoSelect) nextProbeInterval() time.Duration {
	last := s.lastActive.Load()
	if last == 0 || time.Since(time.Unix(0, last)) > s.cfg.idleThreshold {
		return s.cfg.idleInterval
	}
	return s.cfg.activeInterval
}

// TODO: skip on metered connections once lantern-box's platform interface
// gains a network-cost API.
func (s *MutableAutoSelect) runBackgroundLoop() {
	activeInterval := s.cfg.activeInterval
	t := time.NewTicker(activeInterval)
	defer t.Stop()
	if pm := service.FromContext[pause.Manager](s.ctx); pm != nil {
		cb := pause.RegisterTicker(pm, t, activeInterval, nil)
		defer pm.UnregisterCallback(cb)
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			t.Reset(s.nextProbeInterval())
			go s.runProbeCycle(s.ctx)
		}
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
