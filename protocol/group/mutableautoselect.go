package group

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	tun "github.com/sagernet/sing-tun"
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
	"github.com/getlantern/lantern-box/internal/probe"
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
	_ A.ConnectionHandler          = (*MutableAutoSelect)(nil)
	_ A.PacketConnectionHandler    = (*MutableAutoSelect)(nil)
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
	// probeTimeouts overrides behaviorFor's probe timeout by outbound type.
	// SetProbeTimeouts writes it without access held, so it is swapped whole.
	probeTimeouts atomic.Pointer[map[string]time.Duration]
	histories     map[string]*localHistory
	cfg           mutableAutoSelectConfig
	hist          historyParams
	history       adapter.AutoSelectHistoryStorage

	// Scratch buffers reused across calls by rankLocked and splitHealthyForLocked
	// to avoid per-dial allocation; guarded by access, which their callers hold
	// while consuming the returned slice before releasing it.
	scratchPres   []preCandidate
	scratchRanked []rankedCandidate
	scratchSplit  []rankedCandidate
	// scratchDispatch is dispatchProbesLocked's, which never runs inside
	// itself.
	scratchDispatch []string

	// selections holds each network's selection, indexed by streakNet.
	selections [2]networkSelection

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
	// pauseEpoch and pausedSince are guarded by access. pausedSince is when
	// the device or network last paused, or zero while neither is.
	pauseEpoch          uint64
	pausedSince         time.Time
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
	// Guarded by access: a TCP selection whose confirmations failed twice
	// from since, waiting from at for a replacement's probe to pass.
	pendingConfirmedFailure struct {
		tag       string
		since, at time.Time
		// replacementFailed is set once a member tag could move to failed a
		// probe while the switch was pending.
		replacementFailed bool
		// awaitingProbes is set while probes that can decide the switch are
		// requested or running; they may conclude it past
		// confirmStrikeWindow.
		awaitingProbes bool
	}
	// Guarded by access: outageSince is set while an outage of the
	// device's own network is suspected, since the selection and every
	// replacement failed their probes. outageEvidence is when the failures
	// behind the latest suspicion began, and pastOutages the latest
	// maxPastOutages suspicions, each from its evidence to when an answer or
	// a passing probe cleared it.
	outageSince, outageEvidence time.Time
	pastOutages                 []timeSpan
	// Guarded by access: outageRecheck probes for the end of a suspected
	// outage; afterOutage is the TCP selection probed again once another
	// member's pass, completed at at, ended one.
	outageRecheck *time.Timer
	// recheckNext is where in the replacement pool the next recheck starts,
	// so successive rechecks cover every member.
	recheckNext int
	afterOutage struct {
		tag, passed string
		at          time.Time
	}
	// Guarded by access: when a failure's cause last showed the far end
	// answered, which shows the device's own network was up.
	peerAnsweredAt time.Time
	// Guarded by access: replacementWake retries releases that wait on
	// higher-ranked members' probes, at replacementWakeAt.
	replacementWake   *time.Timer
	replacementWakeAt time.Time
	// udpHandles holds each member's open UDP handles, so a member released
	// as the UDP selection for failing can have its stuck sessions closed.
	// Guarded by access.
	udpHandles map[string]map[*dataPlanePacket]struct{}
	// tcpStreams holds each member's open TCP conns, so a member released
	// as the TCP selection for failing can have those still awaiting their
	// first response closed. Guarded by access.
	tcpStreams map[string]map[*dataPlaneStream]struct{}

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

// networkSelection is one network's selected member and the state kept
// about it.
type networkSelection struct {
	// tag is the selected member's tag, "" when unset. It changes only under
	// access but is read without it, as by Now(); a stale tag is harmless:
	// the in-pool check in applyStickiness drops it.
	tag atomic.Value // string
	// Guarded by access: when the selection was last released for
	// consecutive failures.
	lastStreakSwitch time.Time
	// Guarded by access: when the failing probe began that last hard-demoted
	// the selection, which was then released. A delay measured by a probe
	// started earlier says nothing of the network since.
	releasedSince time.Time
	// Guarded by access: the trials of user traffic on another member.
	trials trialState
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
	out.SetProbeTimeouts(options.ProbeTimeoutsMs)
	return out, nil
}

// Probe timeout overrides outside these bounds are ignored: shorter fails a
// healthy member before a handshake can finish, longer holds up a probe wave.
const (
	minProbeTimeoutOverride = 250 * time.Millisecond
	maxProbeTimeoutOverride = 60 * time.Second
)

// SetProbeTimeouts replaces the per-protocol probe timeout overrides; see
// option.MutableAutoSelectOutboundOptions.ProbeTimeoutsMs. Probes already
// running keep the timeout they started with.
func (s *MutableAutoSelect) SetProbeTimeouts(timeoutsMs map[string]uint32) {
	overrides := make(map[string]time.Duration, len(timeoutsMs))
	for typ, ms := range timeoutsMs {
		d := time.Duration(ms) * time.Millisecond
		if d < minProbeTimeoutOverride || d > maxProbeTimeoutOverride {
			s.logger.WarnContext(s.ctx, "ignoring probe timeout override for ", typ, ": ", d)
			continue
		}
		overrides[typ] = d
	}
	s.probeTimeouts.Store(&overrides)
}

// behavior is behaviorFor with the group's probe timeout overrides applied.
func (s *MutableAutoSelect) behavior(outboundType string) protocolBehavior {
	beh := behaviorFor(outboundType)
	if p := s.probeTimeouts.Load(); p != nil {
		if d, ok := (*p)[outboundType]; ok && !beh.excludeFromPool {
			beh.probeTimeout = d
		}
	}
	return beh
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
	h := hydrateLocalHistory(snap, now, s.hist.userFailureWindow)
	s.histories[tag] = h
	age := "unknown"
	if !snap.UpdatedAt.IsZero() {
		age = now.Sub(snap.UpdatedAt).Round(time.Second).String()
	}
	s.logger.Debug("history seed: tag=", tag, " delay=", delayText(snap.LastSuccessDelayMs), " consecutive_failures=",
		snap.ConsecutiveFailures, " user_failures=", len(h.userFailures), " age=", age)
}

// delayText is a delay in milliseconds for a log line, where 0 means never
// measured.
func delayText(ms uint32) string {
	if ms == 0 {
		return "unmeasured"
	}
	return strconv.FormatUint(uint64(ms), 10) + "ms"
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
func (s *MutableAutoSelect) InterfaceUpdated(ctx context.Context) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.isClosed() {
		return
	}
	s.advanceNetworkLocked()
	s.logger.Debug("interface updated: generation=", s.networkGeneration.Load())
	s.dispatchProbesLocked()
}

// silentDestination reports whether a conn on network through origin's
// member, opened at opened, went unanswered for its destination's sake: the
// member answered a conn opened after it, and dest never answered through the
// member, as with a P2P peer that never does. A member going silent on a
// destination it served is still charged.
func (s *MutableAutoSelect) silentDestination(origin connOrigin, opened time.Time, key uint64) bool {
	s.access.Lock()
	defer s.access.Unlock()
	h, ok := s.peekHistoryLocked(origin.out.Tag())
	return ok && h.answeredLater(streakTCP, opened) && !h.tcpDestAnswered(key)
}

// destKeySeed seeds destKey.
var destKeySeed = maphash.MakeSeed()

// destKey is the key a TCP conn's destination is remembered by, which costs
// a conn neither a string nor an allocation.
func destKey(dest M.Socksaddr) uint64 {
	return maphash.Comparable(destKeySeed, dest)
}

// destLogID returns what names a destination in logs, given as a UDP
// destination dest or a TCP destKey key, or "" if neither is known: the same
// for a destination throughout the process, but never its address, which
// logs must not hold.
func destLogID(dest string, key uint64) string {
	if key == 0 {
		if dest == "" {
			return ""
		}
		key = maphash.String(destKeySeed, dest)
	}
	return fmt.Sprintf("%08x", uint32(key>>32))
}

func (s *MutableAutoSelect) pausedBefore(opened time.Time) bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.pausedBeforeLocked(opened)
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
	if t := s.selected(N.NetworkTCP); t != "" {
		return t
	}
	return s.selected(N.NetworkUDP)
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
		s.untrackConnsLocked(tag)
		// A pending move is evidence about this membership only.
		for i := range s.selections {
			if t := &s.selections[i].trials; t.pending == tag {
				t.pending = ""
			}
		}
		delete(s.probeMembers, tag)
		delete(s.urlOverrides, tag)
		delete(s.histories, tag)
		if s.history != nil {
			s.history.Delete(tag)
		}
		s.clearSelectionLocked(tag)
		// So are decisions waiting on it.
		if s.pendingConfirmedFailure.tag == tag {
			s.pendingConfirmedFailure.tag = ""
		}
		if s.afterOutage.tag == tag {
			s.afterOutage.tag = ""
		}
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

// PerformUpdateCheck implements [A.URLTestGroup]. Selection is ranked on
// every dial, so there is no cached choice to refresh.
func (s *MutableAutoSelect) PerformUpdateCheck() {}

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
	conn, err := setUp(ctx, s, streamKind{s, network, destination})
	if err != nil {
		recordError(span, err)
	}
	return conn, err
}

func (s *MutableAutoSelect) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableAutoSelect.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		tagAttr("outbound", s.Now()),
		tagAttr("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	conn, err := setUp(ctx, s, packetKind{s, destination})
	if err != nil {
		recordError(span, err)
	}
	return conn, err
}

// connKind is how setUp sets up and hands out one kind of conn, C. Kinds are
// values rather than closures, so a setup allocates nothing for them.
type connKind[C io.Closer] interface {
	network() string
	// dial sets up a conn through o, for a trial or a failover.
	dial(ctx context.Context, o A.Outbound) (C, error)
	// dialSelection sets up a conn through origin, the selection's, at
	// dialed; a non-nil release goes to wrap.
	dialSelection(ctx context.Context, origin connOrigin, dialed time.Time) (C, func(), error)
	// wrap watches c, set up through origin, and returns what the caller gets.
	wrap(c C, origin connOrigin, release func()) C
	// recheck reports whether a conn through the selection is abandoned,
	// once registered, if its member failed since it was dialed.
	recheck() bool
}

// streamKind is DialContext's conn to destination on network.
type streamKind struct {
	s           *MutableAutoSelect
	net         string
	destination M.Socksaddr
}

func (k streamKind) network() string { return k.net }

func (k streamKind) dial(ctx context.Context, o A.Outbound) (net.Conn, error) {
	return o.DialContext(ctx, k.net, k.destination)
}

func (k streamKind) dialSelection(ctx context.Context, origin connOrigin, dialed time.Time) (net.Conn, func(), error) {
	return k.s.dialPrimary(ctx, origin, k.net, k.destination, dialed)
}

func (k streamKind) wrap(c net.Conn, origin connOrigin, release func()) net.Conn {
	return k.s.wrapStream(c, k.net, k.destination, origin, release)
}

// recheck holds for TCP only: a UDP conn from DialContext isn't tracked, so
// no release sweep passes it by.
func (k streamKind) recheck() bool { return N.NetworkName(k.net) == N.NetworkTCP }

// packetKind is ListenPacket's handle first addressed to destination.
type packetKind struct {
	s           *MutableAutoSelect
	destination M.Socksaddr
}

func (packetKind) network() string { return N.NetworkUDP }

func (k packetKind) dial(ctx context.Context, o A.Outbound) (net.PacketConn, error) {
	return o.ListenPacket(ctx, k.destination)
}

func (k packetKind) dialSelection(ctx context.Context, origin connOrigin, _ time.Time) (net.PacketConn, func(), error) {
	conn, err := k.dial(ctx, origin.out)
	return conn, nil, err
}

func (k packetKind) wrap(c net.PacketConn, origin connOrigin, _ func()) net.PacketConn {
	return k.s.wrapPacket(c, origin, k.destination)
}

// recheck holds because the release sweep closes only sessions already
// waiting on a reply.
func (packetKind) recheck() bool { return true }

// setUp sets up a conn of kind k: through a member on trial if one is due,
// else through the selection, failing over once to another member, and
// starting the ladder if that fails too.
func setUp[C io.Closer, K connKind[C]](ctx context.Context, s *MutableAutoSelect, k K) (C, error) {
	var none C
	network := k.network()
	if err := s.awaitStreakReplacement(ctx, network); err != nil {
		return none, err
	}
	if trial, ok := s.takeTrial(network); ok {
		conn, err := k.dial(ctx, trial.out)
		switch {
		case err == nil && s.originCurrent(trial):
			return k.wrap(conn, trial, nil), nil
		case err == nil:
			// Removed during the setup: no one's trial.
			conn.Close()
		default:
			s.logger.ErrorContext(ctx, err)
			if !errors.Is(ctx.Err(), context.Canceled) && !destinationFailed(err) {
				s.noteTrialFailed(trial, network)
			}
		}
		if ctx.Err() != nil {
			// The trial used up the caller's deadline, so the selection isn't
			// dialed and charged for a setup that had no time to run.
			if err == nil {
				err = ctx.Err()
			}
			return none, err
		}
	}
	origin, err := s.selectOrigin(network, "", primaryRoute)
	if err != nil {
		return none, err
	}
	outerTag, ladderGeneration := origin.out.Tag(), origin.generation
	dialed := time.Now()
	conn, release, err := k.dialSelection(ctx, origin, dialed)
	if !s.noteSelectionSetup(origin, network, err == nil || destinationFailed(err)) {
		// The member was removed during the setup: its result is no one's.
		if err == nil {
			conn.Close()
			if release != nil {
				release()
			}
			err = errAbandonedDial
		}
		return none, err
	}
	// discarded is set for a setup that succeeded, on a member already failing.
	discarded := false
	if err == nil {
		wrapped := k.wrap(conn, origin, release)
		// Registered by now, so a release from here on closes it with the
		// rest; one that came before it was registered is caught here.
		if !k.recheck() || !s.failedSince(outerTag, N.NetworkName(network), dialed) {
			return wrapped, nil
		}
		wrapped.Close()
		err, discarded = errAbandonedDial, true
	}
	s.logger.ErrorContext(ctx, err)
	// A caller giving up says nothing about the member, nor does a server
	// that answered but couldn't reach the destination, nor a device with no
	// network to dial on, so nothing is charged or probed; the caller's
	// deadline expiring does. A device that failed only this member's dial,
	// as with no route to its address, still tries the alternate.
	if errors.Is(ctx.Err(), context.Canceled) || destinationFailed(err) || noNetwork(err) {
		return none, err
	}
	if !discarded {
		s.recordUserFailure(origin, adapter.UserFailureDial, dialed, network, err)
	}

	// Fast failover: try one alternate from the current rank. The active
	// 3-min cadence keeps the probe history fresh enough to pick a working
	// peer without re-probing. The network may have changed during the
	// primary dial, so the alternate carries its own generation. An
	// alternate dialed after the caller's context ended fails at once
	// without saying anything about the alternate, so none is tried.
	altOrigin, altErr := s.selectOrigin(network, outerTag, fallbackRoute)
	if altErr == nil && ctx.Err() == nil {
		dialed = time.Now()
		conn, err = k.dial(ctx, altOrigin.out)
		if err == nil {
			if s.noteFailover(origin, altOrigin, network) {
				return k.wrap(conn, altOrigin, nil), nil
			}
			// Removed during the setup: its conn is no one's.
			conn.Close()
			err = errAbandonedDial
		}
		s.logger.ErrorContext(ctx, err)
		// The primary dial used up most of the caller's deadline, so an
		// alternate cut off by it isn't charged.
		if ctx.Err() == nil {
			s.recordUserFailure(altOrigin, adapter.UserFailureDial, dialed, network, err)
		}
		outerTag, ladderGeneration = altOrigin.out.Tag(), altOrigin.generation
	}

	go s.runLadder(outerTag, ladderGeneration)
	return none, err
}

// routeKind records whether a conn used the selected member or a
// fast-failover alternate.
type routeKind uint8

const (
	primaryRoute routeKind = iota
	fallbackRoute
	// trialRoute is user traffic sent to another member while an outage is
	// suspected, to learn whether the selection alone is failing.
	trialRoute
)

// chargeable reports whether a data-plane failure should count against tag.
// Primary-route failures are counted only while tag is selected for TCP or
// UDP; fallback-route failures are always counted as fresh failover traffic.
func (s *MutableAutoSelect) chargeable(tag string, route routeKind) bool {
	return !s.cfg.demoteOnlySelected || route == fallbackRoute || route == trialRoute ||
		s.selected(N.NetworkTCP) == tag || s.selected(N.NetworkUDP) == tag
}

// wrapStream enables response tracking for TCP only; a conn from
// DialContext("udp") is not tracked, so no release sweep closes it. sing-box
// sets up user UDP that way only for routes with udp_connect, and for a DNS
// server or outbound that dials through the group. A non-nil release runs
// when a TCP conn closes.
func (s *MutableAutoSelect) wrapStream(conn net.Conn, network string, destination M.Socksaddr, origin connOrigin, release func()) net.Conn {
	o := origin.out
	report := s.makeHooks(origin, N.NetworkName(network))
	wrapped := newDataPlaneStream(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, report)
	wrapped.excuseQuietWrites = true
	if N.NetworkName(network) == N.NetworkTCP {
		if destination = destination.Unwrap(); destination.IsValid() {
			report.destKey = destKey(destination)
		}
		wrapped.firstResponseTimeout = s.cfg.dataPlaneFirstResponseTimeout
		wrapped.unansweredAfter = defaultUnansweredAfter
		if report.confirms() {
			wrapped.slowResponseAfter = earlyConfirmAfter
		}
		report.stream, report.release = wrapped, release
	}
	s.access.Lock()
	if N.NetworkName(network) == N.NetworkTCP && s.originCurrentLocked(origin) {
		trackConnLocked(&s.tcpStreams, o.Tag(), wrapped)
	}
	s.access.Unlock()
	return adapter.NewTaggedConn(wrapped, realTag(o))
}

// connOrigin is what a conn was set up through, as of when it was chosen.
// Reports on the conn are made through it, and each is dropped unless its
// membership still holds when the report reaches the group, so a report
// can't land on a member re-added under the same tag.
type connOrigin struct {
	out        A.Outbound
	route      routeKind
	generation uint64
	// membership is out's tag's membership then, or 0 if out wasn't a member.
	membership uint64
	// trialFor is, for a trial, the selection it was made for, and
	// trialForMembership that selection's membership then.
	trialFor           string
	trialForMembership uint64
}

func (s *MutableAutoSelect) originOfLocked(o A.Outbound, route routeKind, generation uint64) connOrigin {
	origin := connOrigin{out: o, route: route, generation: generation}
	if state := s.probeMembers[o.Tag()]; state != nil {
		origin.membership = state.membershipID
	}
	return origin
}

// originCurrent reports whether origin's membership still holds.
func (s *MutableAutoSelect) originCurrent(origin connOrigin) bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.originCurrentLocked(origin)
}

func (s *MutableAutoSelect) originCurrentLocked(origin connOrigin) bool {
	tag := origin.out.Tag()
	out, member := s.members.Load(tag)
	return member && out == origin.out && s.sameMembershipLocked(tag, origin.membership)
}

// wrapPacket watches each exchange on a handle first addressed to
// destination, and tracks the handle until it closes.
func (s *MutableAutoSelect) wrapPacket(conn net.PacketConn, origin connOrigin, destination M.Socksaddr) net.PacketConn {
	o := origin.out
	tag := o.Tag()
	report := s.makeHooks(origin, N.NetworkUDP)
	wrapped := newDataPlanePacket(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, report)
	wrapped.unansweredAfter = defaultUDPUnansweredAfter
	wrapped.unansweredWhileOpen = true
	wrapped.excuseQuietWrites = true
	if destination = destination.Unwrap(); destination.IsValid() {
		wrapped.exchanges.lastDest = destination.String()
	}
	report.packet = wrapped
	s.access.Lock()
	if s.originCurrentLocked(origin) {
		trackConnLocked(&s.udpHandles, tag, wrapped)
	}
	s.access.Unlock()
	return adapter.NewTaggedPacketConn(wrapped, realTag(o))
}

// untrackConnsLocked untracks tag's conns and handles, so a member later
// added under tag isn't swept with them. Caller must hold s.access.
func (s *MutableAutoSelect) untrackConnsLocked(tag string) {
	delete(s.tcpStreams, tag)
	delete(s.udpHandles, tag)
}

// trackConnLocked adds c to tag's conns. Caller must hold s.access.
func trackConnLocked[C comparable](conns *map[string]map[C]struct{}, tag string, c C) {
	if *conns == nil {
		*conns = make(map[string]map[C]struct{})
	}
	if (*conns)[tag] == nil {
		(*conns)[tag] = make(map[C]struct{})
	}
	(*conns)[tag][c] = struct{}{}
}

// untrackConn removes c from tag's conns, one of s's conn sets.
func untrackConn[C comparable](s *MutableAutoSelect, conns *map[string]map[C]struct{}, tag string, c C) {
	s.access.Lock()
	defer s.access.Unlock()
	delete((*conns)[tag], c)
	if len((*conns)[tag]) == 0 {
		delete(*conns, tag)
	}
}

// closeWaitingLocked closes, on another goroutine, tag's conns in conns
// for which waiting holds both now and when closed, and logs how many as
// what. Caller must hold s.access.
func closeWaitingLocked[C interface {
	comparable
	Close() error
}](s *MutableAutoSelect, conns map[string]map[C]struct{}, tag, what string, waiting func(C, time.Time) bool) {
	now := time.Now()
	var picked []C
	for c := range conns[tag] {
		if waiting(c, now) {
			picked = append(picked, c)
		}
	}
	if len(picked) == 0 {
		return
	}
	go func() {
		closed := 0
		for _, c := range picked {
			// A conn answered since it was picked has recovered.
			if waiting(c, time.Now()) {
				c.Close()
				closed++
			}
		}
		if closed > 0 {
			s.logger.Info(what, " closed: tag=", tag, " count=", closed)
		}
	}()
}

// closeDoomedTCPLocked closes tag's TCP conns that have waited at least
// minWait for their first response, once tag has been released as the TCP
// selection for failing, so their callers retry through the new selection
// instead of waiting out their own timeouts. Conns that have answered are
// left alone. Caller must hold s.access; the conns are closed on another
// goroutine.
func (s *MutableAutoSelect) closeDoomedTCPLocked(tag string, minWait time.Duration) {
	closeWaitingLocked(s, s.tcpStreams, tag, "tcp conns", func(conn *dataPlaneStream, now time.Time) bool {
		return conn.awaitingFirstResponse(now, minWait)
	})
}

// closeStuckUDPLocked closes tag's UDP handles whose latest exchange (with
// dest, unless dest is "") went unanswered, once tag has been released
// as the UDP selection for failing. sing-box then rebuilds each session
// through the group's new selection on the app's next datagram. Handles
// still getting replies are left alone, so working flows keep their egress.
// Caller must hold s.access; the handles are closed on another goroutine.
func (s *MutableAutoSelect) closeStuckUDPLocked(tag, dest string) {
	closeWaitingLocked(s, s.udpHandles, tag, "udp sessions", func(handle *dataPlanePacket, now time.Time) bool {
		return handle.awaitingReply(now, dest)
	})
}

// afterUDPFailureLocked acts on a UDP failure of tag. If tag is no longer the
// UDP selection and its failure streak is at the limit, its sessions whose
// exchange went unanswered are closed: those left on it at its release were
// idle or hadn't waited long enough then. Otherwise a release it allows is
// applied. Caller must hold s.access.
func (s *MutableAutoSelect) afterUDPFailureLocked(tag string) {
	if s.selected(N.NetworkUDP) != tag && !s.streakSinceLocked(tag, N.NetworkUDP).IsZero() {
		s.closeStuckUDPLocked(tag, "")
	}
	s.reselectUDPLocked()
}

// failingLocked reports whether tag's UDP failure streak is at the limit, its
// latest probe failed, or a confirmed failure gated it. Caller must hold
// s.access.
func (s *MutableAutoSelect) failingLocked(tag string) bool {
	if !s.streakSinceLocked(tag, N.NetworkUDP).IsZero() {
		return true
	}
	h, ok := s.peekHistoryLocked(tag)
	if !ok {
		return false
	}
	_, consec, _, gated := h.snapshot(time.Now(), s.hist.userFailureWindow)
	return consec > 0 || gated
}

// reselectUDPLocked applies a pending release of the UDP selection for
// consecutive failures without waiting for the next ListenPacket, which an
// app holding one socket never makes. Caller must hold s.access.
func (s *MutableAutoSelect) reselectUDPLocked() {
	sticky := s.selected(N.NetworkUDP)
	if sticky == "" || s.streakSinceLocked(sticky, N.NetworkUDP).IsZero() {
		return
	}
	pool, forNetwork := s.splitHealthyForLocked(s.rankLocked(time.Now()), N.NetworkUDP)
	if len(pool) == 0 {
		return
	}
	if winner, why := s.applyStickiness(N.NetworkUDP, pool, forNetwork); winner.tag != sticky {
		s.logger.Debug("udp candidates: ", describeCandidates(forNetwork))
		s.switchSelectionLocked(N.NetworkUDP, sticky, winner.tag, why)
	}
}

// inheritUDPDestinationsLocked gives next, the new UDP selection, the
// destinations that answered through prev, the one it replaced. Caller must
// hold s.access.
func (s *MutableAutoSelect) inheritUDPDestinationsLocked(prev, next string) {
	from, ok := s.peekHistoryLocked(prev)
	if !ok {
		return
	}
	s.hydrateHistoryLocked(next)
	s.historyForLocked(next).inheritUDPDestinations(from)
}

func (s *MutableAutoSelect) NewConnection(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewPacketConnection(ctx, s, conn, metadata, onClose)
}

// selectOrigin picks the member to set up a conn on network through, other
// than excludeTag, and returns the conn's origin by route, captured as it
// picks it.
func (s *MutableAutoSelect) selectOrigin(network, excludeTag string, route routeKind) (connOrigin, error) {
	if network != N.NetworkTCP && network != N.NetworkUDP {
		return connOrigin{}, fmt.Errorf("network %s not supported", network)
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
		return connOrigin{generation: generation}, errors.New("no outbound available, recovery in progress")
	}
	var winner rankedCandidate
	if excludeTag == "" {
		var why switchReason
		winner, why = s.applyStickiness(network, pool, forNetwork)
		if prev := s.selected(network); prev != winner.tag {
			s.logger.Debug(network, " candidates: ", describeCandidates(forNetwork))
			s.switchSelectionLocked(network, prev, winner.tag, why)
		}
	} else {
		winner = pool[0]
	}
	origin := s.originOfLocked(winner.outbound, route, generation)
	s.access.Unlock()
	return origin, nil
}

// applyStickiness applies switch-tolerance hysteresis between the sticky tag
// and best, the pool's preferred candidate. It looks for the sticky tag
// across every demotion tier, not just the pool, so soft demotion still goes
// through the normal comparison instead of looking like removal.
//
// Retention is capped at softFailLimit failures; beyond that, continuing
// failures release the sticky even if it remains much faster.
//
// A winner other than a set sticky tag comes with why it replaces it.
// Caller must hold s.access.
func (s *MutableAutoSelect) applyStickiness(
	network string,
	pool []rankedCandidate,
	forNetwork []rankedCandidate,
) (winner rankedCandidate, why switchReason) {
	best := pool[0]
	sticky := s.selected(network)
	if sticky == "" {
		return best, switchReason{text: fmt.Sprint(delayText(best.delayMs), ", ", best.demote)}
	}
	if sticky == best.tag {
		// Ranking first among its tier says nothing about a selection whose
		// traffic keeps failing.
		if s.streakReleaseDueLocked(network, best) {
			if replacement, ok := s.releaseForStreakLocked(network, pool, forNetwork, sticky); ok {
				return replacement, switchReason{text: "consecutive failures", released: true}
			}
		}
		return best, switchReason{}
	}
	idx := slices.IndexFunc(forNetwork, func(c rankedCandidate) bool {
		return c.tag == sticky
	})
	if idx < 0 {
		return best, switchReason{text: "not a candidate"}
	}
	c := forNetwork[idx]
	// A measured selection soft-demoted only by its failure streak is
	// released through the streak's safeguards, not to an unmeasured member
	// at once.
	streakOnly := c.delayMs > 0 && best.delayMs == 0 && c.demote == demoteSoft && best.demote < c.demote &&
		c.streak >= consecutiveUserFailureLimit && c.userFails < s.hist.softFailLimit
	// A measured selection isn't given up as unmeasured for a cleaner member
	// whose latest probe failed.
	measuredOverFailed := c.delayMs > 0 && best.probeFailed
	if c.demote == demoteHard && best.demote < demoteHard {
		passed, hold := s.hardDemotionReleaseLocked(sticky, pool)
		if hold {
			return c, switchReason{}
		}
		if passed.tag != "" {
			best = passed
			s.selectionFor(network).releasedSince = s.probeMembers[sticky].outcome.started
		}
	}
	switch {
	// Before the measurement comparison: a hard-demoted selection's release
	// closes its stuck conns whatever either member's delay.
	case c.demote == demoteHard && best.demote < demoteHard:
		return best, switchReason{text: "hard-demoted", released: true}
	case (c.delayMs == 0) != (best.delayMs == 0) && !streakOnly && !measuredOverFailed:
		// The tolerance comparison only means something between two real
		// measurements: delayMs 0 means never measured, and comparing it with
		// a real delay would let a never-probed sticky's 0ms retain it.
		// rankLocked already sorts measured members first, so defer to it.
		return best, switchReason{text: "unmeasured"}
	case c.lastResort && !best.lastResort && best.demote <= c.demote:
		return best, switchReason{text: "last resort no longer needed"}
	case c.demote > best.demote && s.pastRetention(c):
		return best, switchReason{text: "failures past retention", released: true}
	case c.demote >= best.demote && s.streakReleaseDueLocked(network, c):
		if replacement, ok := s.releaseForStreakLocked(network, pool, forNetwork, sticky); ok {
			return replacement, switchReason{text: "consecutive failures", released: true}
		}
		return c, switchReason{}
	case streakOnly:
		return c, switchReason{}
	case s.beatsOnDelay(best.delayMs, c.delayMs) && !best.probeFailed && best.streak < consecutiveUserFailureLimit &&
		!s.probePendingLocked(best.tag) && !s.probedBeforeLocked(best.tag, s.selectionFor(network).releasedSince):
		// A delay from before best's latest, failed probe says nothing about
		// how it would serve now, nor does one of a member whose traffic
		// keeps failing, one being probed again, or one measured before the
		// failure that last released a hard-demoted selection.
		return best, switchReason{text: fmt.Sprint("beaten on delay: ", delayText(best.delayMs), " vs ", delayText(c.delayMs))}
	default:
		return c, switchReason{}
	}
}

// hardDemotionReleaseLocked decides the release of sticky, the selection,
// hard-demoted by a probe that failed: it holds until a member of pool
// passes a probe started since that one, requesting the probes, and
// returns that member to release to once awaitHigherRankedLocked allows. A
// device that lost its own network fails every probe, so releasing sticky
// then only walks the selection off a member that may still work, and a
// pass from before says nothing of now; failures it gathers meanwhile are
// forgiven once the outage is suspected. A member that can't be probed
// counts as passing. Caller must hold s.access.
func (s *MutableAutoSelect) hardDemotionReleaseLocked(sticky string, pool []rankedCandidate) (passed rankedCandidate, hold bool) {
	if !s.latestProbeFailedLocked(sticky) {
		return rankedCandidate{}, false
	}
	since := s.probeMembers[sticky].outcome.started
	var requested []string
	var passedAt time.Time
	found := false
	for _, c := range pool {
		if s.passedStartedSinceLocked(c.tag, since) {
			passed, found, passedAt = c, true, s.probeMembers[c.tag].outcome.completed
			break
		}
		if probe.ValidateURL(s.probeURLForLocked(c.tag)) != nil {
			passed, found = c, true
			break
		}
		// One ranked above the member passing is probed too, so its delay
		// from before isn't taken at face value on the next dial.
		state := s.probeMembers[c.tag]
		if state == nil || state.outcome == nil || state.outcome.started.Before(since) {
			requested = append(requested, c.tag)
		}
	}
	if !found && len(requested) > maxReplacementProbes {
		requested = requested[:maxReplacementProbes]
	}
	s.forceProbesLocked(requested...)
	// One that can't be probed is released to at once.
	if !passedAt.IsZero() && s.awaitHigherRankedLocked(requested, passedAt, since) {
		return rankedCandidate{}, true
	}
	return passed, !found
}

// streakReleaseDueLocked reports whether c, network's selection, is at its
// failure streak's limit and may be released for it. Caller must hold
// s.access.
func (s *MutableAutoSelect) streakReleaseDueLocked(network string, c rankedCandidate) bool {
	return c.streak >= consecutiveUserFailureLimit && s.streakReleaseAllowedLocked(network, c.tag) &&
		!s.streakSinceLocked(c.tag, network).IsZero()
}

// releaseForStreakLocked releases network's selection, sticky, for
// consecutive failures to the member streakReplacementLocked finds, if any,
// for the caller to switch to. Caller must hold s.access.
func (s *MutableAutoSelect) releaseForStreakLocked(network string, pool, forNetwork []rankedCandidate, sticky string) (rankedCandidate, bool) {
	replacement, ok, _ := s.streakReplacementLocked(pool, forNetwork, network, sticky, s.streakSinceLocked(sticky, network))
	if !ok {
		return rankedCandidate{}, false
	}
	// replacement's streak from before the probe that chose it would
	// release it again at once.
	if state := s.probeMembers[replacement.tag]; replacement.streak >= consecutiveUserFailureLimit &&
		state != nil && state.outcome != nil && state.outcome.success {
		if h, ok := s.peekHistoryLocked(replacement.tag); ok {
			h.clearStreakBefore(streakNetFor(network), state.outcome.completed)
		}
	}
	s.selectionFor(network).lastStreakSwitch = time.Now()
	return replacement, true
}

// probeCutoff is the earliest a probe's outcome counts toward a streak
// replacement: a pass from since sticky's streak began, a failure from since.
func probeCutoff(passed bool, since, passedSince time.Time) time.Time {
	if passed {
		return passedSince
	}
	return since
}

// closeStuckOnReleaseLocked closes released's stuck UDP sessions or doomed
// TCP conns when it was released as network's selection for failing.
// Caller must hold s.access.
func (s *MutableAutoSelect) closeStuckOnReleaseLocked(network, released string) {
	if network == N.NetworkUDP {
		s.closeStuckUDPLocked(released, "")
	} else {
		s.closeDoomedTCPLocked(released, earlyConfirmAfter)
	}
}

// streakReleaseAllowedLocked reports whether network's selection, sticky,
// may be released for consecutive failures: once streakSwitchCooldown has
// passed since the last such release, or at once if sticky hasn't answered
// since. Caller must hold s.access.
func (s *MutableAutoSelect) streakReleaseAllowedLocked(network, sticky string) bool {
	last := s.selectionFor(network).lastStreakSwitch
	if last.IsZero() || time.Since(last) >= streakSwitchCooldown {
		return true
	}
	h, ok := s.peekHistoryLocked(sticky)
	return !ok || !h.answeredSince(streakNetFor(network), last)
}

// streakReplacementLocked returns the member to release network's selection,
// sticky, to for consecutive failures since since: the highest-ranked member
// in pool that passed a probe since sticky's streak's first failure, or when
// pool is the clean tier and each of its members failed a probe completed
// since since, such a member of forNetwork's soft tier. It requests probes of
// the others, so a later call may find one, and reports pending while
// awaitHigherRankedLocked holds that member back. A member that can't be probed
// counts as reachable, so a group without probing still releases. When every
// member's fresh probe failed, sticky is probed too unless its result is
// fresh; then sticky's streak is cleared instead of walking the pool, and if
// sticky's probe failed as well, nothing is reachable, as in an outage of the
// device's own network. But when sticky's probe failed and a failure since
// since showed the far end answered, the device is online and the probes are
// what fail, so the highest-ranked other member in pool is returned, unless
// sticky is in pool's tier: nothing then sets that member apart from sticky,
// and when every member refuses, moving on to it only moves the failures.
// Caller must hold s.access.
func (s *MutableAutoSelect) streakReplacementLocked(pool, forNetwork []rankedCandidate, network, sticky string, since time.Time) (replacement rankedCandidate, ok, pending bool) {
	var requested []string
	failed := 0
	sameTier := len(pool) > 0 && slices.ContainsFunc(pool, func(c rankedCandidate) bool { return c.tag == sticky })
	// A member that passed while sticky was failing is as current as one
	// probed once the streak reached its limit; one that failed then is
	// probed again, as its failure may have been the same trouble.
	passedSince := since
	if h, ok := s.peekHistoryLocked(sticky); ok {
		if st, live := h.liveStreak(streakNetFor(network), time.Now(), s.hist.userFailureWindow); live && st.firstAt.Before(since) {
			passedSince = st.firstAt
		}
	}
	for i, tier := range [2][]rankedCandidate{pool, forNetwork} {
		if i == 1 && (len(pool) == 0 || pool[0].demote != demoteClean) {
			break
		}
		// The soft tier's members are probed alongside pool's, but one is
		// returned only once none of pool's is still being probed.
		usable := i == 0 || len(requested) == 0
		for _, c := range tier {
			if c.tag == sticky || (i == 1 && (c.demote != demoteSoft || s.pastRetention(c))) {
				continue
			}
			if probe.ValidateURL(s.probeURLForLocked(c.tag)) != nil {
				if usable {
					return c, true, false
				}
				continue
			}
			if state := s.probeMembers[c.tag]; state != nil && state.outcome != nil && s.outcomeCurrentLocked(c.tag, state.outcome) &&
				!state.outcome.completed.Before(probeCutoff(state.outcome.success, since, passedSince)) {
				if !state.outcome.success {
					failed++
				} else if usable {
					s.forceProbesLocked(requested...)
					if s.awaitHigherRankedLocked(requested, state.outcome.completed, since) {
						return rankedCandidate{}, false, true
					}
					return c, true, false
				}
				continue
			}
			requested = append(requested, c.tag)
		}
	}
	if len(requested) > 0 {
		s.forceProbesLocked(requested...)
	} else if failed > 0 {
		own := s.probeMembers[sticky]
		fresh := own != nil && own.outcome != nil && !own.outcome.completed.Before(since) && s.outcomeCurrentLocked(sticky, own.outcome)
		if !fresh && own != nil && probe.ValidateURL(s.probeURLForLocked(sticky)) == nil {
			s.forceProbesLocked(sticky)
			return rankedCandidate{}, false, true
		}
		if fresh && !own.outcome.success && !s.peerAnsweredAt.Before(since) && !sameTier {
			for _, c := range pool {
				if c.tag != sticky {
					return c, true, false
				}
			}
		}
		if h, ok := s.peekHistoryLocked(sticky); ok {
			h.clearStreak(streakNetFor(network))
		}
		s.logger.Info(network, " consecutive failures on ", sticky, " kept: no other member reachable")
		// A member outside network's pool, such as one without it, passing,
		// or the far end answering shows the device's network is up.
		if fresh && !own.outcome.success && !s.otherMemberPassedSinceLocked(sticky, since) && s.peerAnsweredAt.Before(since) {
			s.suspectOutageLocked(since)
		}
	}
	return rankedCandidate{}, false, len(requested) > 0
}

// Bounds on how long a release waits, after a member passed, for members
// ranked above it whose probes are still requested or running: each is given
// replacementWaitFactor times its delay from its probe's start, within
// [replacementWaitFloor, replacementWaitMax], and none past
// replacementWaitMax from the pass or the release's start, whichever is
// later. A real member's slow probes run about thrice its median delay.
const (
	replacementWaitFactor = 4
	replacementWaitFloor  = 250 * time.Millisecond
	replacementWaitMax    = time.Second
)

// awaitHigherRankedLocked reports whether a release that began at began, to
// a member that passed at passedAt, should wait for above, the members
// ranked over that one without a probe outcome since began. Only those whose
// probes are requested or running count. While it waits, it arms
// replacementWake to retry the release when the earliest wait ends. Caller
// must hold s.access.
func (s *MutableAutoSelect) awaitHigherRankedLocked(above []string, passedAt, began time.Time) bool {
	if len(above) == 0 {
		return false
	}
	now := time.Now()
	end := passedAt
	if began.After(end) {
		end = began
	}
	end = end.Add(replacementWaitMax)
	var until time.Time
	for _, tag := range above {
		attempt := s.runningProbeLocked(tag)
		if state := s.probeMembers[tag]; attempt == nil && (state == nil || state.need == probeNotNeeded) {
			continue
		}
		// One whose probe started before began must be probed again.
		start := now
		if attempt != nil && !attempt.started.Before(began) {
			start = attempt.started
		}
		var delayMs uint32
		if h, ok := s.peekHistoryLocked(tag); ok {
			delayMs = h.rankDelayMs()
		}
		wait := min(max(replacementWaitFactor*time.Duration(delayMs)*time.Millisecond, replacementWaitFloor), replacementWaitMax)
		deadline := start.Add(wait)
		if deadline.After(end) {
			deadline = end
		}
		if deadline.After(now) && (until.IsZero() || deadline.Before(until)) {
			until = deadline
		}
	}
	if until.IsZero() {
		return false
	}
	s.armReplacementWakeLocked(until)
	return true
}

// armReplacementWakeLocked retries waiting releases at at, unless a retry
// is already due no later. Caller must hold s.access.
func (s *MutableAutoSelect) armReplacementWakeLocked(at time.Time) {
	if s.replacementWake != nil {
		if !s.replacementWakeAt.After(at) {
			return
		}
		s.replacementWake.Stop()
	}
	var wake *time.Timer
	wake = time.AfterFunc(time.Until(at), func() {
		s.access.Lock()
		defer s.access.Unlock()
		if s.replacementWake != wake || s.isClosed() {
			return
		}
		s.replacementWake = nil
		s.retryWaitingReleasesLocked()
	})
	s.replacementWake, s.replacementWakeAt = wake, at
}

// retryWaitingReleasesLocked retries what waits on probes: a pending
// confirmed-failure switch, a UDP selection's streak release, and, through
// probeChanged, dials in awaitStreakReplacement. Caller must hold s.access.
func (s *MutableAutoSelect) retryWaitingReleasesLocked() {
	s.tryPendingConfirmedSwitchLocked()
	s.reselectUDPLocked()
	s.notifyProbesLocked()
}

// probeStreakReplacementLocked starts the probes a release of tag as
// network's selection for consecutive failures waits on, as soon as the
// streak allows the release, so they are likely done by the next dial.
// Caller must hold s.access.
func (s *MutableAutoSelect) probeStreakReplacementLocked(tag, network string) {
	since := s.streakSinceLocked(tag, network)
	if since.IsZero() || s.selected(network) != tag || !s.streakReleaseAllowedLocked(network, tag) {
		return
	}
	pool, forNetwork := s.splitHealthyForLocked(s.rankLocked(time.Now()), network)
	s.streakReplacementLocked(pool, forNetwork, network, tag, since)
}

// streakSinceLocked returns when tag's live failure streak on network reached
// consecutiveUserFailureLimit, or zero without such a streak or when it is
// left to confirmations. Probes from then on decide its replacement;
// failures that extend the streak don't make them stale. Caller must hold
// s.access.
func (s *MutableAutoSelect) streakSinceLocked(tag, network string) time.Time {
	h, ok := s.peekHistoryLocked(tag)
	if !ok {
		return time.Time{}
	}
	limit := h.limitReachedAt(streakNetFor(network), time.Now(), s.hist.userFailureWindow)
	if limit.IsZero() || network != N.NetworkTCP {
		return limit
	}
	// A TCP selection whose own probe failed during the streak is left to
	// confirmations under way, which recheck it when its replacements failed
	// too, as in an outage of the device's own network; the streak is for a
	// selection that passes probes while its traffic fails.
	if state := s.probeMembers[tag]; state != nil && state.outcome != nil && !state.outcome.success && s.confirmingLocked(tag) {
		if st, live := h.liveStreak(streakTCP, time.Now(), s.hist.userFailureWindow); live && !st.started.IsZero() && !state.outcome.completed.Before(st.started) {
			return time.Time{}
		}
	}
	return limit
}

// confirmingLocked reports whether confirmations of tag are under way: one
// is pending, one failed awaiting the next, or a switch off tag awaits a
// replacement. Caller must hold s.access.
func (s *MutableAutoSelect) confirmingLocked(tag string) bool {
	state := s.probeMembers[tag]
	return state != nil && (state.confirmationPending || !state.firstConfirmFailedAt.IsZero()) ||
		s.pendingConfirmedFailure.tag == tag
}

// streakProbeWait bounds how long a dial waits for the probes deciding where
// a failing selection is released to.
const streakProbeWait = 2 * time.Second

// awaitStreakReplacement delays a dial while network's selection is at the
// consecutive-failure limit and the probes deciding its replacement are still
// running, for at most streakProbeWait. A dial sent to the failing selection
// meanwhile would most likely fail too. It returns ctx's error if ctx has
// ended, or else net.ErrClosed if the group has closed.
func (s *MutableAutoSelect) awaitStreakReplacement(ctx context.Context, network string) error {
	done := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.isClosed() {
			return net.ErrClosed
		}
		return nil
	}
	var timeout <-chan time.Time
	for {
		s.access.Lock()
		pending, changed := s.startStreakReplacementLocked(network), s.probeChanged
		s.access.Unlock()
		if !pending {
			return done()
		}
		if timeout == nil {
			timer := time.NewTimer(streakProbeWait)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-changed:
		case <-timeout:
			return done()
		case <-ctx.Done():
			return done()
		case <-s.ctx.Done():
			return done()
		}
	}
}

// startStreakReplacementLocked does what streakReplacementLocked does for
// network's selection when applyStickiness would release it for consecutive
// failures: it requests the probes that decide the replacement, or clears the
// selection's streak if every fresh probe failed and no failure since the
// streak began showed the far end answered. It reports whether the
// release is still waiting on probes. Caller must hold s.access.
func (s *MutableAutoSelect) startStreakReplacementLocked(network string) bool {
	sticky := s.selected(network)
	if sticky == "" {
		return false
	}
	since := s.streakSinceLocked(sticky, network)
	if since.IsZero() || !s.streakReleaseAllowedLocked(network, sticky) {
		return false
	}
	pool, forNetwork := s.splitHealthyForLocked(s.rankLocked(time.Now()), network)
	idx := slices.IndexFunc(forNetwork, func(c rankedCandidate) bool { return c.tag == sticky })
	if idx < 0 || len(pool) == 0 || forNetwork[idx].demote < pool[0].demote {
		return false
	}
	_, _, pending := s.streakReplacementLocked(pool, forNetwork, network, sticky, since)
	return pending
}

func (s *MutableAutoSelect) selectionFor(network string) *networkSelection {
	return &s.selections[streakNetFor(network)]
}

// selected returns network's selected tag, or "" if none.
func (s *MutableAutoSelect) selected(network string) string {
	return loadString(&s.selectionFor(network).tag)
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

// describeCandidates names the first few of ranked, in rank order, with what
// ranked them, for a log line.
func describeCandidates(ranked []rankedCandidate) string {
	const shown = 6
	var b strings.Builder
	for i, c := range ranked {
		if i == shown {
			fmt.Fprintf(&b, ", and %d more", len(ranked)-shown)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s %s", c.tag, delayText(c.delayMs), c.demote)
		if c.streak > 0 {
			fmt.Fprintf(&b, " streak=%d", c.streak)
		}
		if c.probeFailed {
			b.WriteString(" probe-failed")
		}
	}
	return b.String()
}

// switchReason is why a network's selection moved off its previous member.
type switchReason struct {
	text string
	// released means the previous member was released for failing, so its
	// stuck conns are closed.
	released bool
}

// switchSelectionLocked makes next network's selection in place of prev and
// logs why; a churn report is only actionable if the logs say which rule
// moved the group. The destinations that answered through a UDP selection
// pass to its successor. Caller must hold s.access.
func (s *MutableAutoSelect) switchSelectionLocked(network, prev, next string, why switchReason) {
	s.selectionFor(network).tag.Store(next)
	if prev == "" {
		s.logger.Info(network, " select: ", next, " (", why.text, ")")
		return
	}
	s.logger.Info(network, " switch: ", prev, " -> ", next, " (", why.text, ")")
	if why.released {
		s.closeStuckOnReleaseLocked(network, prev)
	}
	if network == N.NetworkUDP {
		s.inheritUDPDestinationsLocked(prev, next)
	}
}

// clearSelectionLocked drops the selection of any network where it is tag.
// Unlike a switch it logs nothing and passes no destinations on, since tag is
// no longer a member. Caller must hold s.access.
func (s *MutableAutoSelect) clearSelectionLocked(tag string) {
	for i := range s.selections {
		if slot := &s.selections[i].tag; loadString(slot) == tag {
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

// latestProbeFailedLocked reports whether tag's latest current probe failed,
// even when an outage suspicion forgave the failure so it doesn't demote tag.
// Caller must hold s.access.
func (s *MutableAutoSelect) latestProbeFailedLocked(tag string) bool {
	state := s.probeMembers[tag]
	return state != nil && state.outcome != nil && !state.outcome.success && s.outcomeCurrentLocked(tag, state.outcome)
}

// demoteLevel ranks how cautious selection should be about a candidate.
// Ordering is load-bearing: rankLocked sorts on the integer value, so
// demoteClean must compare < demoteSoft < demoteLastResort < demoteHard for
// the tiers to land in the right order.
type demoteLevel uint8

func (l demoteLevel) String() string {
	switch l {
	case demoteClean:
		return "clean"
	case demoteSoft:
		return "soft"
	case demoteLastResort:
		return "last resort"
	default:
		return "hard"
	}
}

const (
	demoteClean      demoteLevel = iota
	demoteSoft                   // window_count(userFailures) >= 1, hard threshold not reached
	demoteLastResort             // a lastResort member that isn't hard-demoted
	demoteHard                   // consecutive_failures or windowed user-failures at limit
)

type rankedCandidate struct {
	outbound  A.Outbound
	tag       string
	delayMs   uint32
	demote    demoteLevel
	userFails uint32
	streaks   [2]uint32
	// streak is streaks for the network splitHealthyForLocked filtered on.
	streak uint32
	// probeFailed is set when the member's latest probe failed.
	probeFailed     bool
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
	bumped := false
	for _, c := range ranked {
		if !slices.Contains(c.outbound.Network(), network) {
			continue
		}
		c.streak = c.streaks[streakNetFor(network)]
		if c.demote == demoteClean && c.streak >= consecutiveUserFailureLimit {
			c.demote, bumped = demoteSoft, true
		}
		out = append(out, c)
	}
	if bumped {
		slices.SortStableFunc(out, compareRanked)
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
	streaks         [2]uint32
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
		beh := s.behavior(o.Type())
		if beh.excludeFromPool {
			continue
		}
		var (
			rawDelay        uint32
			consec          uint32
			userFails       uint32
			streaks         [2]uint32
			hasRecoveryGate bool
		)
		if h, ok := s.peekHistoryLocked(tag); ok {
			lastDelay, c, uf, gated := h.snapshot(now, s.hist.userFailureWindow)
			rawDelay = lastDelay
			consec = c
			userFails = uint32(uf)
			streaks = h.streakCounts(now, s.hist.userFailureWindow)
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
		pres = append(pres, preCandidate{o: o, tag: tag, delayMs: rawDelay, consec: consec, userFails: userFails, streaks: streaks, lastResort: beh.lastResort, hasRecoveryGate: hasRecoveryGate})
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
		out = append(out, rankedCandidate{outbound: p.o, tag: p.tag, delayMs: p.delayMs, demote: level, userFails: p.userFails, streaks: p.streaks, probeFailed: p.consec > 0 || s.latestProbeFailedLocked(p.tag), lastResort: p.lastResort, hasRecoveryGate: p.hasRecoveryGate})
	}
	s.scratchRanked = out
	slices.SortStableFunc(out, compareRanked)
	return out
}

// compareRanked orders a and b as rankedBefore does, ties equal.
func compareRanked(a, b rankedCandidate) int {
	switch {
	case rankedBefore(a, b):
		return -1
	case rankedBefore(b, a):
		return 1
	}
	return 0
}

func rankedBefore(a, b rankedCandidate) bool {
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
	// Among measured members, one whose latest probe failed ranks after
	// one whose latest passed, whatever their older delays: servers that
	// fail together often rank together.
	if a.probeFailed != b.probeFailed {
		return !a.probeFailed
	}
	return a.delayMs < b.delayMs
}

const lastResortSuccessFreshness = 5 * time.Minute

var probeBound = func(beh protocolBehavior) time.Duration {
	return beh.probeTimeout + 5*time.Second
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

var errAbandonedDial = errors.New("dial abandoned: member failed while it was set up")

// dialPrimary dials through origin, the TCP selection, and probes it, if its
// membership still holds, once the dial has been pending earlyConfirmAfter.
// If that probe fails and the selection moves off it while the dial is still
// pending, it gives up on the dial with errAbandonedDial so the caller fails
// over now rather than at its deadline, and cancels the abandoned dial; a
// conn it returns late is closed. The dial runs under a context derived from
// ctx, which some outbounds tie their conns to, so a TCP conn comes with a
// release that cancels it, to run once the conn closes. If ctx ends first, it
// gives up the same way and returns ctx's error. The dial runs on its own
// goroutine so giving up never waits on an outbound that ignores ctx.
func (s *MutableAutoSelect) dialPrimary(ctx context.Context, origin connOrigin, network string, destination M.Socksaddr, dialed time.Time) (net.Conn, func(), error) {
	o := origin.out
	if network != N.NetworkTCP {
		conn, err := o.DialContext(ctx, network, destination)
		return conn, nil, err
	}
	type result struct {
		conn net.Conn
		err  error
	}
	dialCtx, cancel := context.WithCancel(ctx)
	done := make(chan result, 1)
	go func() {
		conn, err := o.DialContext(dialCtx, network, destination)
		done <- result{conn, err}
	}()
	slow := time.NewTimer(earlyConfirmAfter)
	defer slow.Stop()
	tag := o.Tag()
	abandon := func(err error) (net.Conn, func(), error) {
		cancel()
		go func() {
			if r := <-done; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, nil, err
	}
	for {
		s.access.Lock()
		changed, failed := s.probeChanged, s.failedSinceLocked(tag, N.NetworkTCP, dialed)
		s.access.Unlock()
		if failed {
			return abandon(errAbandonedDial)
		}
		select {
		case r := <-done:
			if r.err != nil {
				cancel()
				return nil, nil, r.err
			}
			return r.conn, cancel, nil
		case <-ctx.Done():
			return abandon(ctx.Err())
		case <-slow.C:
			s.requestEarlyConfirmation(origin)
		case <-changed:
		}
	}
}

// failedSince is failedSinceLocked for a caller not holding s.access.
func (s *MutableAutoSelect) failedSince(tag, network string, since time.Time) bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.failedSinceLocked(tag, network, since)
}

// failedSinceLocked reports whether tag is no longer network's selection and
// is failing: its latest probe, completed after since, failed or, for UDP,
// its failure streak is at the limit. Caller must hold s.access.
func (s *MutableAutoSelect) failedSinceLocked(tag, network string, since time.Time) bool {
	if s.selected(network) == tag {
		return false
	}
	if network == N.NetworkUDP && !s.streakSinceLocked(tag, network).IsZero() {
		return true
	}
	state := s.probeMembers[tag]
	return state != nil && state.outcome != nil && !state.outcome.success &&
		state.outcome.completed.After(since) && s.outcomeCurrentLocked(tag, state.outcome)
}

// deviceFailed reports whether err shows the device failed the conn itself,
// as failedLocally reports it or as noNetwork does: the member isn't at
// fault.
func deviceFailed(err error) bool {
	return failedLocally(err) || noNetwork(err)
}

// noNetwork reports whether err is sing-box finding no interface to dial on,
// which every member would fail alike.
func noNetwork(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, tun.ErrNoRoute) {
		return true
	}
	// sing-box's network-strategy dialer, as on Android, makes this error
	// anew each time; a server relaying its own is the server's failure.
	msg := err.Error()
	return strings.Contains(msg, "no available network interface") && !strings.Contains(msg, "remote error: ")
}

// pausedBeforeLocked reports whether the device or network is paused and a
// conn opened at opened, or a zero time, was open before the pause began:
// its failure may be the pause's. A conn opened during a pause shows the
// device can dial, as when sing-box leaves the network paused after it came
// back. Caller must hold s.access.
func (s *MutableAutoSelect) pausedBeforeLocked(opened time.Time) bool {
	return !s.pausedSince.IsZero() && s.paused() && !opened.After(s.pausedSince)
}

// destinationFailed reports whether err is a server's report that it failed
// to reach the destination, in the form a sing-box server's router gives,
// relayed as Hysteria2 relays it: the server answered, so the error says
// nothing about it.
func destinationFailed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "remote error: open connection to ") ||
		strings.Contains(msg, "remote error: open packet connection to ")
}

// recordUserFailure reports whether a failure of origin's member, on a
// network conn opened at opened, was added to its window. A zero opened means
// unknown. A failure whose cause shows the far end answered ends an outage
// suspicion, and counts unless its conn predates the outage; any other is
// dropped while outage evidence holds, as is one from another network
// generation.
func (s *MutableAutoSelect) recordUserFailure(origin connOrigin, kind adapter.UserFailureKind, opened time.Time, network string, cause error) bool {
	return s.recordUserFailureIf(nil, origin, kind, opened, network, cause)
}

// recordUserFailureIf is recordUserFailure, except that it records nothing
// unless current, called with s.access held, reports true. A nil current
// always does.
func (s *MutableAutoSelect) recordUserFailureIf(current func() bool, origin connOrigin, kind adapter.UserFailureKind, opened time.Time, network string, cause error) bool {
	if s.isClosed() || destinationFailed(cause) || deviceFailed(cause) {
		return false
	}
	s.access.Lock()
	defer s.access.Unlock()
	if (current != nil && !current()) || s.pausedBeforeLocked(opened) {
		return false
	}
	if !s.originCurrentLocked(origin) {
		return false
	}
	tag := origin.out.Tag()
	recorded := s.matchesGenerationLocked(origin.generation) && s.mutateHistoryLocked(tag, func(h *localHistory, now time.Time) bool {
		failure := adapter.UserFailure{At: now, Kind: kind}
		if peerAnswered(cause) {
			s.peerAnsweredAt = now
			// A conn the outage outlasted may have been dropped by the far
			// end meanwhile, so its reset says nothing about the member.
			stale := !opened.IsZero() && (opened.Before(s.outageSince) || opened.Before(s.lastOutageEndLocked()))
			s.endOutageLocked()
			if stale {
				return false
			}
		} else if s.outageEvidenceLocked(opened) {
			return false
		}
		return h.addUserFailure(failure, streakNetFor(network), opened, s.hist.userFailureWindow, s.hist.userFailureDedupeWindow)
	})
	if recorded {
		s.logger.Info("user failure: tag=", tag, " kind=", string(kind))
	}
	s.probeStreakReplacementLocked(tag, network)
	if network == N.NetworkUDP {
		s.afterUDPFailureLocked(tag)
	}
	return recorded
}

// noteUserResponse records that a network conn to dest through origin,
// opened at opened, answered at at. dest is used for UDP only. What it
// updates is in-memory only, so nothing is persisted.
func (s *MutableAutoSelect) noteUserResponse(origin connOrigin, network, dest string, opened, at time.Time) {
	s.noteAnswer(origin, network, dest, 0, opened, at)
}

// noteAnswer is noteUserResponse for a TCP conn whose destination has
// destKey key, or zero if unknown.
func (s *MutableAutoSelect) noteAnswer(origin connOrigin, network, dest string, key uint64, opened, at time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.originCurrentLocked(origin) || !s.matchesGenerationLocked(origin.generation) {
		return
	}
	tag := origin.out.Tag()
	s.hydrateHistoryLocked(tag)
	h := s.historyForLocked(tag)
	h.noteUserResponse(streakNetFor(network), dest, opened, at)
	if key != 0 {
		h.noteTCPAnswered(key, at)
	}
	s.noteSelectionAnsweredLocked(network, tag)
	s.endOutageLocked()
}

// noteUnanswered records that a network conn to dest through origin, opened
// at opened, went unanswered while its caller had been waiting for
// data since waitingSince. It is dropped if another of the member's conns on
// network answered meanwhile: the member was delivering, so the wait was this
// conn's, such as an idle keep-alive conn or a slow destination. A UDP report
// is also dropped unless dest has answered through the member before: a
// destination that never answers looks the same as a dead relay.
func (s *MutableAutoSelect) noteUnanswered(origin connOrigin, network, dest string, opened, waitingSince time.Time) {
	s.noteUnansweredTo(origin, network, dest, 0, opened, waitingSince)
}

// noteUnansweredTo is noteUnanswered for a TCP conn whose destination has
// destKey key, or zero if unknown.
func (s *MutableAutoSelect) noteUnansweredTo(origin connOrigin, network, dest string, key uint64, opened, waitingSince time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	s.noteUnansweredLocked(origin, network, dest, key, opened, waitingSince)
}

// noteUnansweredOffSelection is noteUnanswered for a UDP report that isn't
// chargeable. It counts only for a member that is failing and not the UDP
// selection, so sessions left on it move once its streak reaches the limit,
// as they would on the selection.
func (s *MutableAutoSelect) noteUnansweredOffSelection(origin connOrigin, dest string, opened, waitingSince time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	if tag := origin.out.Tag(); s.selected(N.NetworkUDP) != tag && s.failingLocked(tag) {
		s.noteUnansweredLocked(origin, N.NetworkUDP, dest, 0, opened, waitingSince)
	}
}

// noteUnansweredLocked is noteUnanswered with s.access held.
func (s *MutableAutoSelect) noteUnansweredLocked(origin connOrigin, network, dest string, key uint64, opened, waitingSince time.Time) {
	if s.isClosed() || s.pausedBeforeLocked(opened) || !s.originCurrentLocked(origin) || !s.matchesGenerationLocked(origin.generation) {
		return
	}
	tag := origin.out.Tag()
	s.hydrateHistoryLocked(tag)
	h := s.historyForLocked(tag)
	sn := streakNetFor(network)
	now := time.Now()
	if h.answeredSince(sn, waitingSince) || (sn == streakUDP && !h.udpDestAnswered(dest)) {
		return
	}
	// Before the outage check: trials are for a selection failing while an
	// outage is suspected.
	s.noteSelectionUnansweredLocked(network, tag, dest, opened)
	if s.outageEvidenceLocked(opened) {
		return
	}
	h.addUnanswered(sn, now, opened, s.hist.userFailureWindow)
	msg := network + " unanswered: tag=" + tag
	if id := destLogID(dest, key); id != "" {
		msg += " dest=" + id
	}
	s.logger.Debug(msg + " waited=" + now.Sub(waitingSince).Round(time.Millisecond).String())
	s.probeStreakReplacementLocked(tag, network)
	if sn == streakUDP {
		s.afterUDPFailureLocked(tag)
	}
}

// trySwitchAfterConfirmedFailureLocked moves TCP selection off failedTag,
// whose confirmations failed twice in a row from since, to replacementTag if
// it answered a conn after since, or else a member whose probe passed after
// since. With none, as in an outage of the device's own network, the
// selection stays.
func (s *MutableAutoSelect) trySwitchAfterConfirmedFailureLocked(failedTag, replacementTag string, since time.Time) (switched bool) {
	if s.selected(N.NetworkTCP) != failedTag {
		return false
	}
	if _, member := s.members.Load(failedTag); !member {
		return false
	}
	h, ok := s.peekHistoryLocked(failedTag)
	if !ok {
		return false
	}
	now := time.Now()
	if _, consec, _, _ := h.snapshot(now, s.hist.userFailureWindow); consec == 0 {
		return false
	}
	if replacementTag == "" || !s.eligibleReplacementLocked(now, replacementTag) || !s.answeredSinceLocked(replacementTag, since) {
		replacementTag = s.findProbedReplacementLocked(now, N.NetworkTCP, failedTag, since)
	}
	if replacementTag == "" {
		return false
	}
	last := s.lastConfirmedFailureSwitch
	if !last.at.IsZero() && now.Sub(last.at) < s.cfg.confirmedFailureSwitchCooldown && last.selectedTag != failedTag {
		// A pending switch retries on every probe completion; log it once.
		if s.pendingConfirmedFailure.tag != failedTag {
			s.logger.Info("tcp switch deferred: ", failedTag, " -> ", replacementTag, " (confirmed-failure switch cooldown)")
		}
		return false
	}
	s.switchSelectionLocked(N.NetworkTCP, failedTag, replacementTag, switchReason{text: "failed confirmation"})
	h.startRecoveryGate(now)
	s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = now, replacementTag
	// Its probes failed, so every conn it hasn't answered yet is doomed.
	s.closeDoomedTCPLocked(failedTag, 0)
	return true
}

// tryPendingConfirmedSwitchLocked retries the switch off a TCP selection
// whose confirmations failed twice, as replacement probes complete, probing
// further members while those probed fail. It gives up once the selection
// has moved, confirmStrikeWindow has passed with no probe left that could
// decide it, or every member it could move to failed. Caller must hold
// s.access.
func (s *MutableAutoSelect) tryPendingConfirmedSwitchLocked() {
	p := &s.pendingConfirmedFailure
	if p.tag == "" {
		return
	}
	if s.selected(N.NetworkTCP) != p.tag || (time.Since(p.at) > confirmStrikeWindow && !p.awaitingProbes) {
		p.tag = ""
		return
	}
	defer func() {
		if p.tag != "" {
			p.awaitingProbes = s.probePendingLocked(p.tag) || s.replacementProbesPendingLocked(p.tag)
		}
	}()
	if recheck, abort := s.recheckFailedSelectionLocked(p.tag, p.since, p.replacementFailed); abort {
		p.tag = ""
		return
	} else if recheck {
		return
	}
	switch {
	case s.trySwitchAfterConfirmedFailureLocked(p.tag, "", p.since):
		p.tag = ""
	case s.replacementProbesPendingLocked(p.tag):
	case s.probeTCPReplacementsLocked(p.tag, p.since):
	case s.findProbedReplacementLocked(time.Now(), N.NetworkTCP, p.tag, p.since) != "":
		// The switch cooldown held it back; a probe completing within
		// confirmStrikeWindow retries it.
	default:
		tag, since := p.tag, p.since
		p.tag = ""
		// A member that passed, the selection itself or one that may not take
		// the selection yet, as in its recovery gate, shows the network is up.
		if !s.passedSinceLocked(tag, since) && !s.otherMemberPassedSinceLocked(tag, since) {
			s.suspectOutageLocked(since)
		}
	}
}

// recheckFailedSelectionLocked decides whether tag, whose confirmations
// failed twice from since, still needs probing before a switch. If some
// member tag could move to also failed its probe since, as failed may
// already report, the failures may have been the device's own network, now
// back: a switch then waits for a probe of tag completed after the passing
// replacement's. It reports recheck while that probe is outstanding, and
// abort if it passed. Caller must hold s.access.
func (s *MutableAutoSelect) recheckFailedSelectionLocked(tag string, since time.Time, failed bool) (recheck, abort bool) {
	var passed time.Time
	for _, c := range s.replacementPoolLocked(time.Now(), N.NetworkTCP, tag) {
		state := s.probeMembers[c.tag]
		if state == nil || state.outcome == nil || state.outcome.completed.Before(since) {
			continue
		}
		if !state.outcome.success {
			failed = true
		} else if passed.IsZero() || state.outcome.completed.Before(passed) {
			passed = state.outcome.completed
		}
	}
	if !failed || passed.IsZero() {
		return false, false
	}
	// A probe of tag that started before the pass may have run while the
	// network was still down.
	own := s.probeMembers[tag]
	if own != nil && own.outcome != nil && !own.outcome.started.Before(passed) {
		return false, own.outcome.success
	}
	s.forceProbesLocked(tag)
	return true, false
}

// replacementFailedSinceLocked reports whether a member TCP selection could
// move to from tag has a latest probe that failed no earlier than since.
// Caller must hold s.access.
func (s *MutableAutoSelect) replacementFailedSinceLocked(tag string, since time.Time) bool {
	for _, c := range s.replacementPoolLocked(time.Now(), N.NetworkTCP, tag) {
		state := s.probeMembers[c.tag]
		if state != nil && state.outcome != nil && !state.outcome.success && !state.outcome.completed.Before(since) {
			return true
		}
	}
	return false
}

// otherMemberPassedSinceLocked reports whether a member other than tag has a
// latest probe that passed no earlier than since, whether or not the
// selection could move to it. Caller must hold s.access.
func (s *MutableAutoSelect) otherMemberPassedSinceLocked(tag string, since time.Time) bool {
	for _, other := range s.tags {
		if _, member := s.members.Load(other); member && other != tag && s.passedSinceLocked(other, since) {
			return true
		}
	}
	return false
}

// suspectOutageLocked holds off confirmations until an answer or a passing
// probe: probing on through an outage only piles up failures that
// hard-demote every member. evidence is when the failures that led here
// began.
func (s *MutableAutoSelect) suspectOutageLocked(evidence time.Time) {
	if s.outageSince.IsZero() {
		s.outageSince, s.outageEvidence = time.Now(), evidence
		s.armOutageRecheckLocked()
		s.logger.Info("no member reachable: holding off confirmations")
		// Those failures were the network's, not the members'; left counted
		// they hard-demote members that still work, or release a selection
		// once the outage ends.
		now := time.Now()
		for tag, h := range s.histories {
			h.clearStreaks()
			if h.forgiveProbeFailures(evidence) && s.history != nil {
				s.history.Store(tag, h.toTagHistory(now, s.hist))
			}
		}
	}
}

func (s *MutableAutoSelect) outageSuspectedLocked() bool {
	return !s.outageSince.IsZero()
}

// trialInterval spaces trials of user traffic on another member.
const trialInterval = 2 * time.Second

// trialRetryAfter spaces the trial after one that failed, doubling with each
// further failure up to trialRetryMax. A failed trial most likely means every
// member is down, and a trial made as the network returns only takes a
// request off the selection, but one dead member mustn't end the search.
const (
	trialRetryAfter = 30 * time.Second
	trialRetryMax   = 5 * time.Minute
)

// trialState tracks the trials of user traffic on another member made for
// one network's selection, tag, while the probes can't tell whether the
// selection alone is failing, as when the probe endpoint is down too.
type trialState struct {
	tag        string
	membership uint64
	// unansweredAt is when the selection last went unanswered, and setupOK
	// whether its latest setup succeeded and its traffic since hasn't
	// failed locally: a device that lost its own network fails its conns
	// itself, so trials then would learn nothing.
	unansweredAt time.Time
	setupOK      bool
	// lastAt is when the latest trial was made, and next the rank of the
	// member the next one goes to, so trials move on past one that failed.
	lastAt time.Time
	next   int
	// failures counts trials that failed since the selection last answered.
	failures int
	// pending is the member whose trial first answered at pendingAt, and
	// last at pendingLast, after one failed; the selection moves to it if a
	// conn of its own opened after pendingAt still goes unanswered while
	// pendingLast is fresh.
	pending                string
	pendingAt, pendingLast time.Time
}

// trialsLocked returns the trial state of network's selection, starting it
// afresh when the selection is another member, or another membership of its
// tag, than it was kept for. Caller must hold s.access.
func (s *MutableAutoSelect) trialsLocked(network string) *trialState {
	t := &s.selectionFor(network).trials
	sticky := s.selected(network)
	var membership uint64
	if state := s.probeMembers[sticky]; state != nil {
		membership = state.membershipID
	}
	if t.tag != sticky || t.membership != membership {
		*t = trialState{tag: sticky, membership: membership}
	}
	return t
}

// takeTrial returns the member to send this conn on network to as a trial
// for the selection, trialFor, if one is due: while an outage is suspected
// or the probes can't decide, the selection's setups succeed and its
// traffic went unanswered within probeFreshnessWindow, at most every
// trialInterval, and after a failed trial only as trialRetryAfter allows.
func (s *MutableAutoSelect) takeTrial(network string) (trial connOrigin, ok bool) {
	s.access.Lock()
	defer s.access.Unlock()
	sticky := s.selected(network)
	t := s.trialsLocked(network)
	now := time.Now()
	if sticky == "" || !s.trialDueLocked(network, sticky, t, now) {
		return connOrigin{}, false
	}
	pool := s.replacementPoolLocked(now, network, sticky)
	if len(pool) == 0 {
		return connOrigin{}, false
	}
	c := pool[t.next%len(pool)]
	t.next++
	t.lastAt = now
	s.logger.Debug(network, " trial: ", c.tag, " (selection ", sticky, " unanswered)")
	trial = s.originOfLocked(c.outbound, trialRoute, s.networkGeneration.Load())
	trial.trialFor, trial.trialForMembership = sticky, t.membership
	return trial, true
}

// trialDueLocked reports whether a trial for sticky, network's selection
// with trial state t, is due at now. Caller must hold s.access.
func (s *MutableAutoSelect) trialDueLocked(network, sticky string, t *trialState, now time.Time) bool {
	// Bounded so the shift can't overflow.
	spacing := trialInterval
	if t.failures > 0 {
		spacing = min(trialRetryAfter<<min(t.failures-1, 8), trialRetryMax)
	}
	return t.setupOK && now.Sub(t.unansweredAt) <= probeFreshnessWindow && now.Sub(t.lastAt) >= spacing &&
		(s.outageSuspectedLocked() || s.probesCantDecideLocked(network, sticky))
}

// probesCantDecideLocked reports whether the latest probe of sticky,
// network's selection, failed while no member it could move to has passed
// one since that probe started: until one does, probes can't tell whether
// the selection alone is failing. Caller must hold s.access.
func (s *MutableAutoSelect) probesCantDecideLocked(network, sticky string) bool {
	if !s.latestProbeFailedLocked(sticky) {
		return false
	}
	since := s.probeMembers[sticky].outcome.started
	pool := s.replacementPoolLocked(time.Now(), network, sticky)
	return len(pool) > 0 && !slices.ContainsFunc(pool, func(c rankedCandidate) bool { return s.passedSinceLocked(c.tag, since) })
}

// trialForLocked returns network's trial state if it is still that of
// trialFor under membership ID membership and generation is current, or nil.
// Caller must hold s.access.
func (s *MutableAutoSelect) trialForLocked(network, trialFor string, membership, generation uint64) *trialState {
	if t := s.trialsLocked(network); t.tag == trialFor && t.membership == membership && s.matchesGenerationLocked(generation) {
		return t
	}
	return nil
}

// noteSelectionSetup records whether a setup on network through origin, the
// selection's, succeeded, and reports whether origin's membership still
// holds; if not, nothing is recorded. A conn the device itself failed counts
// as a failed setup: trials stop until the next one succeeds.
func (s *MutableAutoSelect) noteSelectionSetup(origin connOrigin, network string, ok bool) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.originCurrentLocked(origin) {
		return false
	}
	if t := s.trialForLocked(network, origin.out.Tag(), origin.membership, origin.generation); t != nil {
		t.setupOK = ok
	}
	return true
}

// noteFailover reports whether alt's membership still holds, so a conn on
// network set up through it, failing over from the selection's origin,
// serves. For TCP it also confirms origin's member, with alt's as the
// replacement, if origin's membership still holds too.
func (s *MutableAutoSelect) noteFailover(origin, alt connOrigin, network string) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.originCurrentLocked(alt) {
		return false
	}
	if network == N.NetworkTCP && s.originCurrentLocked(origin) {
		s.requestFailureConfirmationLocked(origin.out.Tag(), alt.out.Tag(), origin.generation)
	}
	return true
}

// noteTrialAnswered acts on a trial conn on network through origin answering
// while the selection's traffic went unanswered within probeFreshnessWindow:
// origin's member serves while the selection doesn't, so the selection moves
// to it. After a failed trial the answer may only mean the network is back,
// so the move waits for the selection to leave a conn opened after the answer
// unanswered.
func (s *MutableAutoSelect) noteTrialAnswered(origin connOrigin, network string) {
	s.access.Lock()
	defer s.access.Unlock()
	tag := origin.out.Tag()
	t := s.trialForLocked(network, origin.trialFor, origin.trialForMembership, origin.generation)
	if t == nil || t.tag == "" || t.tag == tag || t.unansweredAt.IsZero() || time.Since(t.unansweredAt) > probeFreshnessWindow {
		return
	}
	if !s.originCurrentLocked(origin) {
		return
	}
	if t.failures > 0 {
		// A later answer from the same member doesn't push the move back.
		now := time.Now()
		if t.pending != tag {
			t.pending, t.pendingAt = tag, now
		}
		t.pendingLast = now
		return
	}
	s.moveToTrialLocked(network, t.tag, tag)
}

// noteTrialFailed records that a trial on network through origin failed: a
// pending move to origin's member is dropped.
func (s *MutableAutoSelect) noteTrialFailed(origin connOrigin, network string) {
	s.access.Lock()
	defer s.access.Unlock()
	s.noteTrialFailedLocked(origin, network)
}

// noteTrialUnanswered is noteTrialFailed, once per failed, for a trial on
// network through origin that went unanswered from waitingSince, unless its
// member has answered since: the report is then obsolete, as
// noteUnansweredLocked finds too.
func (s *MutableAutoSelect) noteTrialUnanswered(origin connOrigin, network string, waitingSince time.Time, failed *atomic.Bool) {
	s.access.Lock()
	defer s.access.Unlock()
	if !s.originCurrentLocked(origin) {
		return
	}
	if h, ok := s.peekHistoryLocked(origin.out.Tag()); ok && h.answeredSince(streakNetFor(network), waitingSince) {
		return
	}
	if failed.CompareAndSwap(false, true) {
		s.noteTrialFailedLocked(origin, network)
	}
}

func (s *MutableAutoSelect) noteTrialFailedLocked(origin connOrigin, network string) {
	if !s.originCurrentLocked(origin) {
		return
	}
	if t := s.trialForLocked(network, origin.trialFor, origin.trialForMembership, origin.generation); t != nil {
		t.failures++
		if t.pending == origin.out.Tag() {
			t.pending = ""
		}
	}
}

// noteSelectionUnansweredLocked records that tag, if network's selection,
// left a conn to dest opened at opened unanswered: a pending trial member
// takes the selection if the conn was opened after its trial answered. A UDP
// selection with a trial due and a member to try has its sessions stuck on
// dest closed, so an app holding one socket sets up a new session, which the
// trial takes. Caller must hold s.access.
func (s *MutableAutoSelect) noteSelectionUnansweredLocked(network, tag, dest string, opened time.Time) {
	t := s.trialsLocked(network)
	if t.tag != tag {
		return
	}
	now := time.Now()
	t.unansweredAt = now
	if t.pending != "" && opened.After(t.pendingAt) {
		pending := t.pending
		t.pending = ""
		if now.Sub(t.pendingLast) <= probeFreshnessWindow {
			s.moveToTrialLocked(network, tag, pending)
			return
		}
	}
	if network == N.NetworkUDP && s.trialDueLocked(network, tag, t, now) && len(s.replacementPoolLocked(now, network, tag)) > 0 {
		s.closeStuckUDPLocked(tag, dest)
	}
}

// noteSelectionAnsweredLocked records that tag, if network's selection,
// answered: it serves, so nothing calls for a trial, and a pending trial
// member is dropped. Caller must hold s.access.
func (s *MutableAutoSelect) noteSelectionAnsweredLocked(network, tag string) {
	if t := s.trialsLocked(network); t.tag == tag {
		t.unansweredAt, t.failures, t.pending = time.Time{}, 0, ""
	}
}

// moveToTrialLocked moves network's selection from prev to tag, whose trial
// answered. Caller must hold s.access.
func (s *MutableAutoSelect) moveToTrialLocked(network, prev, tag string) {
	if _, member := s.members.Load(tag); !member {
		return
	}
	s.switchSelectionLocked(network, prev, tag, switchReason{text: "trial answered", released: true})
	// Its trial's setup succeeded, and an app may keep using that handle.
	s.trialsLocked(network).setupOK = true
}

// outageRecheckInterval spaces the probes that look for the end of a
// suspected outage. Without them only the periodic cycle, minutes apart,
// would notice a member coming back while the selection stays down.
const outageRecheckInterval = 5 * time.Second

// armOutageRecheckLocked probes, every outageRecheckInterval while an
// outage is suspected and background probing runs, the TCP selection and the
// members it would move to, a few at a time in turn. Their failures are forgiven, and a pass ends the
// suspicion. A pause stops it and probing going idle ends it; waking and
// probing resuming arm it again. Caller must hold s.access.
func (s *MutableAutoSelect) armOutageRecheckLocked() {
	if s.outageRecheck != nil {
		s.outageRecheck.Stop()
	}
	s.outageRecheck = time.AfterFunc(outageRecheckInterval, func() {
		s.access.Lock()
		defer s.access.Unlock()
		if s.isClosed() || s.paused() || !s.probing || !s.outageSuspectedLocked() {
			return
		}
		sticky := s.selected(N.NetworkTCP)
		var tags []string
		if sticky != "" {
			tags = append(tags, sticky)
		}
		pool := s.replacementPoolLocked(time.Now(), N.NetworkTCP, sticky)
		for i := range min(len(pool), maxReplacementProbes) {
			tags = append(tags, pool[(s.recheckNext+i)%len(pool)].tag)
		}
		s.recheckNext += maxReplacementProbes
		s.forceProbesLocked(tags...)
		s.armOutageRecheckLocked()
	})
}

// probeSelectionAfterOutageLocked probes the TCP selection again once
// passed's pass, completed at at, ended a suspected outage: the selection's
// own failures until then may have been the outage's, or its own. Caller
// must hold s.access.
func (s *MutableAutoSelect) probeSelectionAfterOutageLocked(passed string, at time.Time) {
	sticky := s.selected(N.NetworkTCP)
	if sticky == "" || sticky == passed {
		return
	}
	s.afterOutage.tag, s.afterOutage.passed, s.afterOutage.at = sticky, passed, at
	s.forceProbesLocked(sticky)
}

// settleAfterOutageLocked moves the TCP selection, tag, to the member whose
// pass ended the outage if tag's probe, started after that pass, failed: the
// network was up for it as for the pass. A failure of one started earlier
// may have been the outage's, so tag is probed again. Once tag has answered
// traffic since the pass, it serves, and it stays. Caller must hold
// s.access.
func (s *MutableAutoSelect) settleAfterOutageLocked(tag string, outcome *probeOutcome) {
	a := &s.afterOutage
	if a.tag == "" || a.tag != tag {
		return
	}
	if s.answeredSinceLocked(tag, a.at) {
		a.tag = ""
		return
	}
	if outcome.started.Before(a.at) {
		if outcome.success {
			a.tag = ""
		} else {
			s.requestProbesLocked([]string{tag}, probeForced, false)
		}
		return
	}
	passed, at := a.passed, a.at
	a.tag = ""
	if !outcome.success && !s.trySwitchAfterConfirmedFailureLocked(tag, passed, at) &&
		s.pendingConfirmedFailure.tag == "" && s.replacementProbesPendingLocked(tag) {
		// A member still being probed may be what holds the switch back; the
		// pending switch retries it as they complete.
		p := &s.pendingConfirmedFailure
		p.tag, p.since, p.at, p.replacementFailed = tag, at, outcome.completed, false
	}
}

type timeSpan struct{ from, to time.Time }

// maxPastOutages bounds the past outages kept: a probe outlives few.
const maxPastOutages = 4

// startedInOutageLocked reports whether a probe started at started ran into
// an outage of the device's own network: one suspected now, or a past one,
// from its evidence to its end. Caller must hold s.access.
func (s *MutableAutoSelect) startedInOutageLocked(started time.Time) bool {
	return s.outageSuspectedLocked() || slices.ContainsFunc(s.pastOutages, func(o timeSpan) bool {
		return !started.Before(o.from) && started.Before(o.to)
	})
}

// endOutageLocked clears an outage suspicion once something answered.
func (s *MutableAutoSelect) endOutageLocked() {
	if !s.outageSince.IsZero() {
		s.pastOutages = append(s.pastOutages, timeSpan{s.outageEvidence, time.Now()})
		s.outageSince = time.Time{}
		if len(s.pastOutages) > maxPastOutages {
			s.pastOutages = s.pastOutages[1:]
		}
		if s.outageRecheck != nil {
			s.outageRecheck.Stop()
		}
		s.logger.Info("member reachable again: confirmations resume")
	}
}

// lastOutageEndLocked returns when the latest outage suspicion was cleared,
// or zero if none was. Caller must hold s.access.
func (s *MutableAutoSelect) lastOutageEndLocked() time.Time {
	if len(s.pastOutages) == 0 {
		return time.Time{}
	}
	return s.pastOutages[len(s.pastOutages)-1].to
}

// outageEvidenceLocked reports whether a failure on a conn opened at opened
// says more about an outage of the device's own network than about its
// member: an outage is suspected, or the conn was opened before the latest
// one ended. Such failures aren't counted. Caller must hold s.access.
func (s *MutableAutoSelect) outageEvidenceLocked(opened time.Time) bool {
	return s.outageSuspectedLocked() || (!opened.IsZero() && opened.Before(s.lastOutageEndLocked()))
}

// replacementProbesPendingLocked reports whether a member TCP selection could
// move to from tag has a probe requested or running. Caller must hold
// s.access.
func (s *MutableAutoSelect) replacementProbesPendingLocked(tag string) bool {
	for _, c := range s.replacementPoolLocked(time.Now(), N.NetworkTCP, tag) {
		if s.probePendingLocked(c.tag) {
			return true
		}
	}
	return false
}

// probePendingLocked reports whether tag has a probe requested or running.
// Caller must hold s.access.
func (s *MutableAutoSelect) probePendingLocked(tag string) bool {
	if state := s.probeMembers[tag]; state != nil && state.need != probeNotNeeded {
		return true
	}
	return s.runningProbeLocked(tag) != nil
}

// runningProbeLocked returns tag's probe attempt still running, or nil. The
// attempt being accepted is still in flight but finalized, so not running.
// Caller must hold s.access.
func (s *MutableAutoSelect) runningProbeLocked(tag string) *probeAttempt {
	out, member := s.members.Load(tag)
	if !member {
		return nil
	}
	if attempt := s.inFlightProbes[out]; attempt != nil && !attempt.finalized {
		return attempt
	}
	return nil
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

// passedSinceLocked reports whether tag's latest probe passed and completed
// no earlier than since; a zero since sets no bound. Caller must hold
// s.access.
func (s *MutableAutoSelect) passedSinceLocked(tag string, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	state := s.probeMembers[tag]
	return state != nil && state.outcome != nil && state.outcome.success &&
		!state.outcome.completed.Before(since) && s.outcomeCurrentLocked(tag, state.outcome)
}

// probedBeforeLocked reports whether tag's latest probe started before t,
// or it has none; a zero t is before every probe. Caller must hold s.access.
func (s *MutableAutoSelect) probedBeforeLocked(tag string, t time.Time) bool {
	if t.IsZero() {
		return false
	}
	state := s.probeMembers[tag]
	return state == nil || state.outcome == nil || state.outcome.started.Before(t)
}

// passedStartedSinceLocked reports whether tag's latest probe passed and
// started no earlier than since, so it tested the network as it was from
// since on. Caller must hold s.access.
func (s *MutableAutoSelect) passedStartedSinceLocked(tag string, since time.Time) bool {
	state := s.probeMembers[tag]
	return state != nil && state.outcome != nil && state.outcome.success &&
		!state.outcome.started.Before(since) && s.outcomeCurrentLocked(tag, state.outcome)
}

// answeredSinceLocked reports whether one of tag's TCP conns was answered
// after since. A fast-failover dial succeeding doesn't count: a member that
// blackholes traffic still accepts connections. Caller must hold s.access.
func (s *MutableAutoSelect) answeredSinceLocked(tag string, since time.Time) bool {
	h, ok := s.peekHistoryLocked(tag)
	return ok && h.answeredSince(streakTCP, since)
}

// findProbedReplacementLocked returns the highest-ranked member of
// replacementPoolLocked whose latest probe passed no earlier than since and
// that has no recovery gate, or "" if there is none or
// awaitHigherRankedLocked holds it back. A soft-demoted member is
// returned only once every clean one has a probe outcome since since. Caller
// must hold s.access.
func (s *MutableAutoSelect) findProbedReplacementLocked(now time.Time, network, excludeTag string, since time.Time) string {
	cleanUndecided := false
	var above []string
	for _, c := range s.replacementPoolLocked(now, network, excludeTag) {
		state := s.probeMembers[c.tag]
		undecided := state == nil || state.outcome == nil || state.outcome.completed.Before(since)
		if c.demote == demoteClean {
			cleanUndecided = cleanUndecided || undecided
		} else if cleanUndecided {
			break
		}
		if s.eligibleReplacementLocked(now, c.tag) && s.passedSinceLocked(c.tag, since) {
			// A zero since passes a member with no outcome.
			passedAt := now
			if !undecided {
				passedAt = state.outcome.completed
			}
			if s.awaitHigherRankedLocked(above, passedAt, since) {
				return ""
			}
			return c.tag
		}
		if undecided {
			above = append(above, c.tag)
		}
	}
	return ""
}

// replacementPoolLocked returns the members network's selection could move
// to from tag, in rank order: network's healthiest tier after excluding tag,
// followed by its soft tier when that tier is clean, since a failed probe
// leaves a member clean. A member past softFailLimit is left out of that
// appended tier, since applyStickiness releases one to a cleaner tier at
// once; a pool that is itself the soft tier keeps it, as nothing cleaner
// exists to release it to. The result aliases scratch space reused by the
// next split. Caller must hold s.access.
func (s *MutableAutoSelect) replacementPoolLocked(now time.Time, network, tag string) []rankedCandidate {
	ranked := slices.DeleteFunc(s.rankLocked(now), func(c rankedCandidate) bool {
		return c.tag == tag
	})
	pool, forNetwork := s.splitHealthyForLocked(ranked, network)
	if len(pool) == 0 || pool[0].demote != demoteClean {
		return pool
	}
	n := len(pool)
	for n < len(forNetwork) && forNetwork[n].demote == demoteSoft {
		n++
	}
	soft := slices.DeleteFunc(forNetwork[len(pool):n], s.pastRetention)
	return forNetwork[:len(pool)+len(soft)]
}

// pastRetention reports whether c has more failures than applyStickiness
// retains a selection through.
func (s *MutableAutoSelect) pastRetention(c rankedCandidate) bool {
	return c.userFails > s.hist.softFailLimit
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
