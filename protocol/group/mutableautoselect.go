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

	// reportMu orders a conn's reports against Remove detaching its member's
	// conns: a report checks and reaches the group under the read lock, so it
	// can't land on a member re-added under the same tag. Taken before access.
	reportMu     sync.RWMutex
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
	// replacement failed their probes, and outageEndedAt when the suspicion
	// was last cleared by an answer or a passing probe. outageEvidence is
	// when the failures behind the latest suspicion began, and pastOutages
	// the latest maxPastOutages suspicions, each from its evidence to its
	// end.
	outageSince, outageEndedAt, outageEvidence time.Time
	pastOutages                                []timeSpan
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
	// Guarded by access: when each network's selection was last released
	// for consecutive failures.
	lastStreakSwitch struct{ tcp, udp time.Time }
	// Guarded by access: each network's trials of user traffic on another
	// member, indexed by streakNet.
	trials [2]trialState
	// Guarded by access: per streakNet, when the failing probe began that
	// last hard-demoted the selection, which was then released. A delay
	// measured by a probe started earlier says nothing of the network since.
	releasedSince [2]time.Time
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
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
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
		s.detachConnsLocked(tag)
		// A pending move is evidence about this membership only.
		for i := range s.trials {
			if s.trials[i].pending == tag {
				s.trials[i].pending = ""
			}
		}
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
	if err := s.awaitStreakReplacement(ctx, network); err != nil {
		recordError(span, err)
		return nil, err
	}
	if trial, ok := s.takeTrial(network); ok {
		conn, err := trial.out.DialContext(ctx, network, destination)
		switch {
		case !s.originCurrent(trial):
			// Removed during the setup: no one's trial.
			if err == nil {
				conn.Close()
			}
		case err == nil:
			return s.wrapStream(conn, network, trial, nil), nil
		default:
			s.logger.ErrorContext(ctx, err)
			if !errors.Is(ctx.Err(), context.Canceled) {
				s.noteTrialFailed(trial.out.Tag(), network, trial.trialFor, trial.generation)
			}
		}
		if ctx.Err() != nil {
			// The trial used up the caller's deadline, so the selection isn't
			// dialed and charged for a setup that had no time to run.
			if err == nil {
				err = ctx.Err()
			}
			recordError(span, err)
			return nil, err
		}
	}
	origin, err := s.selectOrigin(network, "", primaryRoute)
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	o, generation := origin.out, origin.generation
	outerTag := o.Tag()
	dialed := time.Now()
	conn, release, err := s.dialPrimary(ctx, o, network, destination, generation, dialed)
	if !s.originCurrent(origin) {
		// The member was removed during the setup: its result is no one's.
		if err == nil {
			conn.Close()
			if release != nil {
				release()
			}
			err = errAbandonedDial
		}
		recordError(span, err)
		return nil, err
	}
	s.noteSelectionSetup(network, outerTag, generation, err == nil)
	if err == nil {
		stream := s.wrapStream(conn, network, origin, release)
		// Registered by now, so a switch from here on closes it with the
		// rest; one that came before it was registered is caught here.
		if N.NetworkName(network) != N.NetworkTCP || !s.failedSince(outerTag, N.NetworkTCP, dialed) {
			return stream, nil
		}
		stream.Close()
		err = errAbandonedDial
	}
	s.logger.ErrorContext(ctx, err)
	// A caller giving up says nothing about the member, so nothing is
	// charged or probed; its deadline expiring does.
	if errors.Is(ctx.Err(), context.Canceled) {
		recordError(span, err)
		return nil, err
	}
	// Attribute the failure to the outer (member) tag so rankLocked sees
	// it. For nested groups, the inner tag isn't a member of this group,
	// so recordUserFailure would drop on the member gate.
	s.recordUserFailure(outerTag, adapter.UserFailureDial, generation, dialed, network, err)

	// Fast failover: try one alternate from the current rank. The active
	// 3-min cadence keeps the probe history fresh enough to pick a working
	// peer without re-probing. The network may have changed during the
	// primary dial, so the alternate carries its own generation. An
	// alternate dialed after the caller's context ended fails at once
	// without saying anything about the alternate, so none is tried.
	ladderGeneration := generation
	altOrigin, altErr := s.selectOrigin(network, outerTag, fallbackRoute)
	alt, altGeneration := altOrigin.out, altOrigin.generation
	if altErr == nil && ctx.Err() == nil {
		replacementTag := alt.Tag()
		dialed = time.Now()
		conn, err = alt.DialContext(ctx, network, destination)
		current := s.originCurrent(altOrigin)
		if err == nil && current {
			if network == N.NetworkTCP {
				s.requestFailureConfirmation(outerTag, replacementTag, generation)
			}
			return s.wrapStream(conn, network, altOrigin, nil), nil
		}
		if err == nil {
			// Removed during the setup: its conn is no one's.
			conn.Close()
			err = errAbandonedDial
		}
		s.logger.ErrorContext(ctx, err)
		// The primary dial used up most of the caller's deadline, so an
		// alternate cut off by it isn't charged.
		if ctx.Err() == nil && current {
			s.recordUserFailure(replacementTag, adapter.UserFailureDial, altGeneration, dialed, network, err)
		}
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
	if err := s.awaitStreakReplacement(ctx, N.NetworkUDP); err != nil {
		recordError(span, err)
		return nil, err
	}
	if trial, ok := s.takeTrial(N.NetworkUDP); ok {
		conn, err := trial.out.ListenPacket(ctx, destination)
		switch {
		case !s.originCurrent(trial):
			// Removed during the setup: no one's trial.
			if err == nil {
				conn.Close()
			}
		case err == nil:
			return s.wrapPacket(conn, trial, destination), nil
		default:
			s.logger.ErrorContext(ctx, err)
			if !errors.Is(ctx.Err(), context.Canceled) {
				s.noteTrialFailed(trial.out.Tag(), N.NetworkUDP, trial.trialFor, trial.generation)
			}
		}
		if ctx.Err() != nil {
			if err == nil {
				err = ctx.Err()
			}
			recordError(span, err)
			return nil, err
		}
	}
	origin, err := s.selectOrigin("udp", "", primaryRoute)
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	o, generation := origin.out, origin.generation
	outerTag := o.Tag()
	dialed := time.Now()
	conn, err := o.ListenPacket(ctx, destination)
	if !s.originCurrent(origin) {
		// The member was removed during the setup: its result is no one's.
		if err == nil {
			conn.Close()
			err = errAbandonedDial
		}
		recordError(span, err)
		return nil, err
	}
	s.noteSelectionSetup(N.NetworkUDP, outerTag, generation, err == nil)
	if err == nil {
		pc := s.wrapPacket(conn, origin, destination)
		// The release sweep closes only sessions already waiting on a reply,
		// so one set up across it is caught here.
		if !s.failedSince(outerTag, N.NetworkUDP, dialed) {
			return pc, nil
		}
		pc.Close()
		err = errAbandonedDial
	}
	s.logger.ErrorContext(ctx, err)
	if errors.Is(ctx.Err(), context.Canceled) {
		recordError(span, err)
		return nil, err
	}
	s.recordUserFailure(outerTag, adapter.UserFailureDial, generation, dialed, N.NetworkUDP, err)

	ladderGeneration := generation
	altOrigin, altErr := s.selectOrigin("udp", outerTag, fallbackRoute)
	alt, altGeneration := altOrigin.out, altOrigin.generation
	if altErr == nil && ctx.Err() == nil {
		replacementTag := alt.Tag()
		dialed = time.Now()
		conn, err = alt.ListenPacket(ctx, destination)
		current := s.originCurrent(altOrigin)
		if err == nil && current {
			return s.wrapPacket(conn, altOrigin, destination), nil
		}
		if err == nil {
			// Removed during the setup: its session is no one's.
			conn.Close()
			err = errAbandonedDial
		}
		s.logger.ErrorContext(ctx, err)
		// The primary dial used up most of the caller's deadline, so an
		// alternate cut off by it isn't charged.
		if ctx.Err() == nil && current {
			s.recordUserFailure(replacementTag, adapter.UserFailureDial, altGeneration, dialed, N.NetworkUDP, err)
		}
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
	// trialRoute is user traffic sent to another member while an outage is
	// suspected, to learn whether the selection alone is failing.
	trialRoute
)

// chargeable reports whether a data-plane failure should count against tag.
// Primary-route failures are counted only while tag is selected for TCP or
// UDP; fallback-route failures are always counted as fresh failover traffic.
func (s *MutableAutoSelect) chargeable(tag string, route routeKind) bool {
	return !s.cfg.demoteOnlySelected || route == fallbackRoute || route == trialRoute ||
		loadString(&s.stickyTag.tcp) == tag || loadString(&s.stickyTag.udp) == tag
}

// wrapStream enables response tracking for TCP only; a conn from
// DialContext("udp") is not tracked. A non-nil release runs when a TCP conn
// closes.
func (s *MutableAutoSelect) wrapStream(conn net.Conn, network string, origin connOrigin, release func()) net.Conn {
	o := origin.out
	hooks := s.makeHooks(o.Tag(), N.NetworkName(network), origin.route, origin.generation, origin.trialFor)
	wrapped := newDataPlaneStream(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	if N.NetworkName(network) == N.NetworkTCP {
		wrapped.firstResponseTimeout = s.cfg.dataPlaneFirstResponseTimeout
		wrapped.unansweredAfter = defaultUnansweredAfter
		wrapped.slowResponseAfter = earlyConfirmAfter
		wrapped.reportQuiet = true
		tag := o.Tag()
		wrapped.hooks.onClose = func() {
			s.untrackTCPStream(tag, wrapped)
			if release != nil {
				release()
			}
		}
	}
	s.access.Lock()
	switch {
	case !s.attachLocked(origin, wrapped.hooks.detached):
	case N.NetworkName(network) == N.NetworkTCP:
		tag := o.Tag()
		if s.tcpStreams == nil {
			s.tcpStreams = make(map[string]map[*dataPlaneStream]struct{})
		}
		if s.tcpStreams[tag] == nil {
			s.tcpStreams[tag] = make(map[*dataPlaneStream]struct{})
		}
		s.tcpStreams[tag][wrapped] = struct{}{}
	}
	s.access.Unlock()
	return adapter.NewTaggedConn(wrapped, realTag(o))
}

// connOrigin is what a conn was set up through, as of when it was chosen.
type connOrigin struct {
	out        A.Outbound
	route      routeKind
	generation uint64
	// membership is out's tag's membership then, or 0 if unknown.
	membership uint64
	// trialFor is, for a trial, the selection it was made for.
	trialFor string
}

// originOf returns the origin of a conn about to be set up through o.
func (s *MutableAutoSelect) originOf(o A.Outbound, route routeKind, generation uint64) connOrigin {
	s.access.Lock()
	defer s.access.Unlock()
	return s.originOfLocked(o, route, generation)
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
	return member && out == origin.out && (origin.membership == 0 || s.sameMembershipLocked(tag, origin.membership))
}

// attachLocked reports whether origin's membership still holds, so a conn
// set up through it attaches to the member; if not, as when the tag was
// removed and maybe re-added during the setup, the conn's hooks are
// detached. Caller must hold s.access.
func (s *MutableAutoSelect) attachLocked(origin connOrigin, detached *atomic.Bool) bool {
	if s.originCurrentLocked(origin) {
		return true
	}
	detached.Store(true)
	return false
}

// wrapPacket watches each exchange on a handle first addressed to
// destination, and tracks the handle until it closes.
func (s *MutableAutoSelect) wrapPacket(conn net.PacketConn, origin connOrigin, destination M.Socksaddr) net.PacketConn {
	o := origin.out
	tag := o.Tag()
	hooks := s.makeHooks(tag, N.NetworkUDP, origin.route, origin.generation, origin.trialFor)
	wrapped := newDataPlanePacket(conn, s.cfg.dataPlaneIdle, s.cfg.dataPlaneProvedRead, hooks)
	wrapped.unansweredAfter = defaultUDPUnansweredAfter
	wrapped.unansweredWhileOpen = true
	if destination = destination.Unwrap(); destination.IsValid() {
		wrapped.lastDest = destination.String()
	}
	wrapped.hooks.onClose = func() { s.untrackUDPHandle(tag, wrapped) }
	if origin.route == trialRoute {
		var answered atomic.Bool
		response, unanswered := wrapped.hooks.onResponse, wrapped.hooks.onUnanswered
		wrapped.hooks.onResponse = func(at time.Time, dest string) {
			answered.Store(true)
			response(at, dest)
		}
		wrapped.hooks.onUnanswered = func(waitingSince time.Time, dest string) {
			unanswered(waitingSince, dest)
			// A trial that never answered closes, so an app holding its
			// socket sets up again rather than staying on a member that
			// failed; one that answered serves its app.
			if !answered.Load() {
				wrapped.Close()
			}
		}
	}
	s.access.Lock()
	if s.attachLocked(origin, wrapped.hooks.detached) {
		if s.udpHandles == nil {
			s.udpHandles = make(map[string]map[*dataPlanePacket]struct{})
		}
		if s.udpHandles[tag] == nil {
			s.udpHandles[tag] = make(map[*dataPlanePacket]struct{})
		}
		s.udpHandles[tag][wrapped] = struct{}{}
	}
	s.access.Unlock()
	return adapter.NewTaggedPacketConn(wrapped, realTag(o))
}

// detachConnsLocked stops tag's conns and handles from reporting to the
// group and untracks them, so a member later added under tag starts without
// their evidence and isn't swept with them. Caller must hold s.access.
func (s *MutableAutoSelect) detachConnsLocked(tag string) {
	for conn := range s.tcpStreams[tag] {
		conn.hooks.detached.Store(true)
	}
	for handle := range s.udpHandles[tag] {
		handle.hooks.detached.Store(true)
	}
	delete(s.tcpStreams, tag)
	delete(s.udpHandles, tag)
}

func (s *MutableAutoSelect) untrackTCPStream(tag string, conn *dataPlaneStream) {
	s.access.Lock()
	defer s.access.Unlock()
	delete(s.tcpStreams[tag], conn)
	if len(s.tcpStreams[tag]) == 0 {
		delete(s.tcpStreams, tag)
	}
}

// closeDoomedTCPLocked closes tag's TCP conns that have waited at least
// minWait for their first response, once tag has been released as the TCP
// selection for failing, so their callers retry through the new selection
// instead of waiting out their own timeouts. Conns that have answered are
// left alone. Caller must hold s.access; the conns are closed on another
// goroutine.
func (s *MutableAutoSelect) closeDoomedTCPLocked(tag string, minWait time.Duration) {
	now := time.Now()
	var doomed []*dataPlaneStream
	for conn := range s.tcpStreams[tag] {
		if conn.awaitingFirstResponse(now, minWait) {
			doomed = append(doomed, conn)
		}
	}
	if len(doomed) == 0 {
		return
	}
	go func() {
		closed := 0
		for _, conn := range doomed {
			// A conn answered since it was picked has recovered.
			if conn.awaitingFirstResponse(time.Now(), minWait) {
				conn.Close()
				closed++
			}
		}
		if closed > 0 {
			s.logger.Info("tcp conns closed: tag=", tag, " count=", closed)
		}
	}()
}

func (s *MutableAutoSelect) untrackUDPHandle(tag string, handle *dataPlanePacket) {
	s.access.Lock()
	defer s.access.Unlock()
	delete(s.udpHandles[tag], handle)
	if len(s.udpHandles[tag]) == 0 {
		delete(s.udpHandles, tag)
	}
}

// closeStuckUDPLocked closes tag's UDP handles whose latest exchange went
// unanswered, once tag has been released as the UDP selection for failing.
// sing-box then rebuilds each session through the group's new selection on
// the app's next datagram. Handles still getting replies are left alone, so
// working flows keep their egress. Caller must hold s.access; the handles
// are closed on another goroutine.
func (s *MutableAutoSelect) closeStuckUDPLocked(tag string) {
	s.closeStuckUDPToLocked(tag, "")
}

// closeStuckUDPToLocked is closeStuckUDPLocked for the handles whose latest
// exchange went to dest, or any if dest is "". Caller must hold s.access.
func (s *MutableAutoSelect) closeStuckUDPToLocked(tag, dest string) {
	now := time.Now()
	waiting := func(handle *dataPlanePacket, now time.Time) bool {
		if dest == "" {
			return handle.awaitingReply(now)
		}
		return handle.awaitingReplyFrom(now, dest)
	}
	var stuck []*dataPlanePacket
	for handle := range s.udpHandles[tag] {
		if waiting(handle, now) {
			stuck = append(stuck, handle)
		}
	}
	if len(stuck) == 0 {
		return
	}
	go func() {
		closed := 0
		for _, handle := range stuck {
			// A handle answered since it was picked has recovered.
			if waiting(handle, time.Now()) {
				handle.Close()
				closed++
			}
		}
		if closed > 0 {
			s.logger.Info("udp sessions closed: tag=", tag, " count=", closed)
		}
	}()
}

// afterUDPFailureLocked acts on a UDP failure of tag. If tag is no longer the
// UDP selection and its failure streak is at the limit, its sessions whose
// exchange went unanswered are closed: those left on it at its release were
// idle or hadn't waited long enough then. Otherwise a release it allows is
// applied. Caller must hold s.access.
func (s *MutableAutoSelect) afterUDPFailureLocked(tag string) {
	if loadString(&s.stickyTag.udp) != tag && !s.streakSinceLocked(tag, N.NetworkUDP).IsZero() {
		s.closeStuckUDPLocked(tag)
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
	slot := &s.stickyTag.udp
	sticky := loadString(slot)
	if sticky == "" || s.streakSinceLocked(sticky, N.NetworkUDP).IsZero() {
		return
	}
	pool, forNetwork := s.splitHealthyForLocked(s.rankLocked(time.Now()), N.NetworkUDP)
	if len(pool) == 0 {
		return
	}
	if winner := s.applyStickiness(N.NetworkUDP, slot, pool, forNetwork); winner.tag != sticky {
		slot.Store(winner.tag)
		s.inheritUDPDestinationsLocked(sticky, winner.tag)
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

func (s *MutableAutoSelect) selectOutbound(network, excludeTag string) (A.Outbound, uint64, error) {
	origin, err := s.selectOrigin(network, excludeTag, primaryRoute)
	return origin.out, origin.generation, err
}

// selectOrigin is selectOutbound returning the origin of a conn set up
// through the member it picks, by route, captured as it picks it.
func (s *MutableAutoSelect) selectOrigin(network, excludeTag string, route routeKind) (connOrigin, error) {
	var slot *atomic.Value
	switch network {
	case "tcp":
		slot = &s.stickyTag.tcp
	case "udp":
		slot = &s.stickyTag.udp
	default:
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
		winner = s.applyStickiness(network, slot, pool, forNetwork)
		prev := loadString(slot)
		if prev != winner.tag {
			slot.Store(winner.tag)
			if prev == "" {
				s.logger.Info(network, " select: ", winner.tag)
			} else if network == N.NetworkUDP {
				s.inheritUDPDestinationsLocked(prev, winner.tag)
			}
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
// Caller must hold s.access.
func (s *MutableAutoSelect) applyStickiness(
	network string,
	slot *atomic.Value,
	pool []rankedCandidate,
	forNetwork []rankedCandidate,
) rankedCandidate {
	best := pool[0]
	sticky := loadString(slot)
	if sticky == "" {
		return best
	}
	if sticky == best.tag {
		// Ranking first among its tier says nothing about a selection whose
		// traffic keeps failing.
		if s.streakReleaseDueLocked(network, best) {
			if replacement, ok := s.releaseForStreakLocked(network, pool, forNetwork, sticky); ok {
				return replacement
			}
		}
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
			return c
		}
		if passed.tag != "" {
			best = passed
			s.releasedSince[streakNetFor(network)] = s.probeMembers[sticky].outcome.started
		}
	}
	switch {
	case (c.delayMs == 0) != (best.delayMs == 0) && !streakOnly && !measuredOverFailed:
		// The tolerance comparison only means something between two real
		// measurements: delayMs 0 means never measured, and comparing it with
		// a real delay would let a never-probed sticky's 0ms retain it.
		// rankLocked already sorts measured members first, so defer to it.
		s.logSwitch(network, sticky, best, "unmeasured")
	case c.demote == demoteHard && best.demote < demoteHard:
		s.logSwitch(network, sticky, best, "hard-demoted")
		s.closeStuckOnReleaseLocked(network, sticky)
	case c.lastResort && !best.lastResort && best.demote <= c.demote:
		s.logSwitch(network, sticky, best, "last resort no longer needed")
	case c.demote > best.demote && s.pastRetention(c):
		s.logSwitch(network, sticky, best, "failures past retention")
		s.closeStuckOnReleaseLocked(network, sticky)
	case c.demote >= best.demote && s.streakReleaseDueLocked(network, c):
		if replacement, ok := s.releaseForStreakLocked(network, pool, forNetwork, sticky); ok {
			return replacement
		}
		return c
	case streakOnly:
		return c
	case s.beatsOnDelay(best.delayMs, c.delayMs) && !best.probeFailed && best.streak < consecutiveUserFailureLimit &&
		!s.probePendingLocked(best.tag) && !s.probedBeforeLocked(best.tag, s.releasedSince[streakNetFor(network)]):
		// A delay from before best's latest, failed probe says nothing about
		// how it would serve now, nor does one of a member whose traffic
		// keeps failing, one being probed again, or one measured before the
		// failure that last released a hard-demoted selection.
		s.logSwitch(network, sticky, best, "beaten on delay")
	default:
		return c
	}
	return best
}

// hardDemotionReleaseLocked decides the release of sticky, the selection,
// hard-demoted by a probe that failed: it holds until a member of pool
// passes a probe started since that one, requesting the probes, and
// returns that member to release to. A device that lost its own network
// fails every probe, so releasing sticky then only walks the selection off
// a member that may still work, and a pass from before says nothing of now;
// failures it gathers meanwhile are forgiven once the outage is suspected.
// A member that can't be probed counts as passing. Caller must hold
// s.access.
func (s *MutableAutoSelect) hardDemotionReleaseLocked(sticky string, pool []rankedCandidate) (passed rankedCandidate, hold bool) {
	if !s.latestProbeFailedLocked(sticky) {
		return rankedCandidate{}, false
	}
	since := s.probeMembers[sticky].outcome.started
	var requested []string
	found := false
	for _, c := range pool {
		if s.passedStartedSinceLocked(c.tag, since) || probe.ValidateURL(s.probeURLForLocked(c.tag)) != nil {
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
	if len(requested) > 0 {
		s.requestProbesLocked(requested, probeForced, false)
		s.dispatchProbesLocked()
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
// consecutive failures to the member streakReplacementLocked finds, if any.
// Caller must hold s.access.
func (s *MutableAutoSelect) releaseForStreakLocked(network string, pool, forNetwork []rankedCandidate, sticky string) (rankedCandidate, bool) {
	replacement, ok, _ := s.streakReplacementLocked(pool, forNetwork, network, sticky, s.streakSinceLocked(sticky, network))
	if !ok {
		return rankedCandidate{}, false
	}
	s.logSwitch(network, sticky, replacement, "consecutive failures")
	// replacement's streak from before the probe that chose it would
	// release it again at once.
	if state := s.probeMembers[replacement.tag]; replacement.streak >= consecutiveUserFailureLimit &&
		state != nil && state.outcome != nil && state.outcome.success {
		if h, ok := s.peekHistoryLocked(replacement.tag); ok {
			h.clearStreakBefore(streakNetFor(network), state.outcome.completed)
		}
	}
	*s.lastStreakSwitchLocked(network) = time.Now()
	s.closeStuckOnReleaseLocked(network, sticky)
	return replacement, true
}

// closeStuckOnReleaseLocked closes released's stuck UDP sessions or doomed
// TCP conns when it was released as network's selection for failing.
// Caller must hold s.access.
func (s *MutableAutoSelect) closeStuckOnReleaseLocked(network, released string) {
	if network == N.NetworkUDP {
		s.closeStuckUDPLocked(released)
	} else {
		s.closeDoomedTCPLocked(released, earlyConfirmAfter)
	}
}

// streakReleaseAllowedLocked reports whether network's selection, sticky,
// may be released for consecutive failures: once streakSwitchCooldown has
// passed since the last such release, or at once if sticky hasn't answered
// since. Caller must hold s.access.
func (s *MutableAutoSelect) streakReleaseAllowedLocked(network, sticky string) bool {
	last := *s.lastStreakSwitchLocked(network)
	if last.IsZero() || time.Since(last) >= streakSwitchCooldown {
		return true
	}
	h, ok := s.peekHistoryLocked(sticky)
	return !ok || !h.answeredSince(streakNetFor(network), last)
}

// streakReplacementLocked returns the member to release network's selection,
// sticky, to for consecutive failures since since: the highest-ranked member
// in pool whose probe, completed no earlier than since, passed, or when pool
// is the clean tier and each of its members failed, such a member of
// forNetwork's soft tier. It requests probes of the others, so a later call
// may find one. A member that can't be probed counts as reachable, so a group
// without probing still releases. When every member's fresh probe failed,
// sticky is probed too unless its result is fresh; then sticky's streak is
// cleared instead of walking the pool, and if sticky's probe failed as well,
// nothing is reachable, as in an outage of the device's own network. But when
// sticky's probe failed and a failure since since showed the far end
// answered, the device is online and the probes are what fail, so the
// highest-ranked other member in pool is returned, unless sticky is in pool's
// tier: nothing then sets that member apart from sticky, and when every
// member refuses, moving on to it only moves the failures. Caller must hold
// s.access.
func (s *MutableAutoSelect) streakReplacementLocked(pool, forNetwork []rankedCandidate, network, sticky string, since time.Time) (replacement rankedCandidate, ok, pending bool) {
	var requested []string
	failed := 0
	sameTier := len(pool) > 0 && slices.ContainsFunc(pool, func(c rankedCandidate) bool { return c.tag == sticky })
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
			if state := s.probeMembers[c.tag]; state != nil && state.outcome != nil &&
				!state.outcome.completed.Before(since) && s.outcomeCurrentLocked(c.tag, state.outcome) {
				if !state.outcome.success {
					failed++
				} else if usable {
					return c, true, false
				}
				continue
			}
			requested = append(requested, c.tag)
		}
	}
	if len(requested) > 0 {
		s.requestProbesLocked(requested, probeForced, false)
		s.dispatchProbesLocked()
	} else if failed > 0 {
		own := s.probeMembers[sticky]
		fresh := own != nil && own.outcome != nil && !own.outcome.completed.Before(since) && s.outcomeCurrentLocked(sticky, own.outcome)
		if !fresh && own != nil && probe.ValidateURL(s.probeURLForLocked(sticky)) == nil {
			s.requestProbesLocked([]string{sticky}, probeForced, false)
			s.dispatchProbesLocked()
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

// probeStreakReplacementLocked starts the probes a release of tag as
// network's selection for consecutive failures waits on, as soon as the
// streak allows the release, so they are likely done by the next dial.
// Caller must hold s.access.
func (s *MutableAutoSelect) probeStreakReplacementLocked(tag, network string) {
	since := s.streakSinceLocked(tag, network)
	if since.IsZero() || loadString(s.stickySlot(network)) != tag || !s.streakReleaseAllowedLocked(network, tag) {
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
		started := h.streakStarted(streakTCP, time.Now(), s.hist.userFailureWindow)
		if !started.IsZero() && !state.outcome.completed.Before(started) {
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
	sticky := loadString(s.stickySlot(network))
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

func (s *MutableAutoSelect) stickySlot(network string) *atomic.Value {
	if network == N.NetworkUDP {
		return &s.stickyTag.udp
	}
	return &s.stickyTag.tcp
}

func (s *MutableAutoSelect) lastStreakSwitchLocked(network string) *time.Time {
	if network == N.NetworkUDP {
		return &s.lastStreakSwitch.udp
	}
	return &s.lastStreakSwitch.tcp
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
		sort.SliceStable(out, func(i, j int) bool { return rankedBefore(out[i], out[j]) })
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
			userFails = uint32(len(uf))
			for n := range streaks {
				streaks[n] = h.streakCount(streakNet(n), now, s.hist.userFailureWindow)
			}
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
	sort.SliceStable(out, func(i, j int) bool { return rankedBefore(out[i], out[j]) })
	return out
}

// rankedBefore orders candidates by tier, then within a tier.
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

var errAbandonedDial = errors.New("dial abandoned: member failed while it was set up")

// dialPrimary dials o, the TCP selection, and probes o once the dial has been
// pending earlyConfirmAfter. If that probe fails and the selection moves off
// o while the dial is still pending, it gives up on the dial with
// errAbandonedDial so the caller fails over now rather than at its deadline,
// and cancels the abandoned dial; a conn it returns late is closed. The dial
// runs under a context derived from ctx, which some outbounds tie their conns
// to, so a TCP conn comes with a release that cancels it, to run once the conn
// closes.
func (s *MutableAutoSelect) dialPrimary(ctx context.Context, o A.Outbound, network string, destination M.Socksaddr, generation uint64, dialed time.Time) (net.Conn, func(), error) {
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
	for {
		s.access.Lock()
		changed, failed := s.probeChanged, s.failedSinceLocked(tag, N.NetworkTCP, dialed)
		s.access.Unlock()
		if failed {
			cancel()
			go func() {
				if r := <-done; r.conn != nil {
					r.conn.Close()
				}
			}()
			return nil, nil, errAbandonedDial
		}
		select {
		case r := <-done:
			if r.err != nil {
				cancel()
				return nil, nil, r.err
			}
			return r.conn, cancel, nil
		case <-slow.C:
			s.requestEarlyConfirmation(tag, generation)
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
	if loadString(s.stickySlot(network)) == tag {
		return false
	}
	if network == N.NetworkUDP && !s.streakSinceLocked(tag, network).IsZero() {
		return true
	}
	state := s.probeMembers[tag]
	return state != nil && state.outcome != nil && !state.outcome.success &&
		state.outcome.completed.After(since) && s.outcomeCurrentLocked(tag, state.outcome)
}

// recordUserFailure reports whether a current member's failure, on a network
// conn opened at opened, was added to its window. A zero opened means
// unknown. A failure whose cause shows the far end answered ends an outage
// suspicion, and counts unless its conn predates the outage; any other is
// dropped while outage evidence holds.
func (s *MutableAutoSelect) recordUserFailure(tag string, kind adapter.UserFailureKind, generation uint64, opened time.Time, network string, cause error) bool {
	recorded := s.mutateHistory(tag, generation, func(h *localHistory, now time.Time) bool {
		failure := adapter.UserFailure{At: now, Kind: kind}
		// mutateHistory holds s.access, so a suspicion can't start and clear
		// the streaks between this check and the add.
		if peerAnswered(cause) {
			s.peerAnsweredAt = now
			// A conn the outage outlasted may have been dropped by the far
			// end meanwhile, so its reset says nothing about the member.
			stale := !opened.IsZero() && (opened.Before(s.outageSince) || opened.Before(s.outageEndedAt))
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
	s.access.Lock()
	s.probeStreakReplacementLocked(tag, network)
	if network == N.NetworkUDP {
		s.afterUDPFailureLocked(tag)
	}
	s.access.Unlock()
	return recorded
}

// noteUserResponse records that a member's network conn to dest, opened at
// opened, answered at at. dest is used for UDP only. What it updates is
// in-memory only, so nothing is persisted.
func (s *MutableAutoSelect) noteUserResponse(tag string, generation uint64, network, dest string, opened, at time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	if _, member := s.members.Load(tag); !member || !s.matchesGenerationLocked(generation) {
		return
	}
	s.hydrateHistoryLocked(tag)
	s.historyForLocked(tag).noteUserResponse(streakNetFor(network), dest, opened, at)
	s.noteSelectionAnsweredLocked(network, tag)
	s.endOutageLocked()
}

// noteUnanswered records that a current member's network conn to dest,
// opened at opened, went unanswered while its caller had been waiting for
// data since waitingSince. It is dropped if another of the member's conns on
// network answered meanwhile: the member was delivering, so the wait was this
// conn's, such as an idle keep-alive conn or a slow destination. A UDP report
// is also dropped unless dest has answered through the member before: a
// destination that never answers looks the same as a dead relay.
func (s *MutableAutoSelect) noteUnanswered(tag string, generation uint64, network, dest string, opened, waitingSince time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	s.noteUnansweredLocked(tag, generation, network, dest, opened, waitingSince)
}

// noteUnansweredOffSelection is noteUnanswered for a UDP report that isn't
// chargeable. It counts only for a member that is failing and not the UDP
// selection, so sessions left on it move once its streak reaches the limit,
// as they would on the selection.
func (s *MutableAutoSelect) noteUnansweredOffSelection(tag string, generation uint64, dest string, opened, waitingSince time.Time) {
	s.access.Lock()
	defer s.access.Unlock()
	if loadString(&s.stickyTag.udp) != tag && s.failingLocked(tag) {
		s.noteUnansweredLocked(tag, generation, N.NetworkUDP, dest, opened, waitingSince)
	}
}

// noteUnansweredLocked is noteUnanswered with s.access held.
func (s *MutableAutoSelect) noteUnansweredLocked(tag string, generation uint64, network, dest string, opened, waitingSince time.Time) {
	if _, member := s.members.Load(tag); !member || !s.matchesGenerationLocked(generation) {
		return
	}
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
	s.logger.Debug(network, " unanswered: tag=", tag)
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
	slot := &s.stickyTag.tcp
	if loadString(slot) != failedTag {
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
	slot.Store(replacementTag)
	h.startRecoveryGate(now)
	s.lastConfirmedFailureSwitch.at, s.lastConfirmedFailureSwitch.selectedTag = now, replacementTag
	s.logger.Info("tcp switch: ", failedTag, " -> ", replacementTag, " (failed confirmation)")
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
	if loadString(&s.stickyTag.tcp) != p.tag || (time.Since(p.at) > confirmStrikeWindow && !p.awaitingProbes) {
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
	case s.probeTCPReplacementsLocked(p.tag, p.since) > 0:
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
	s.requestProbesLocked([]string{tag}, probeForced, false)
	s.dispatchProbesLocked()
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
			h.clearStreak(streakTCP)
			h.clearStreak(streakUDP)
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
	tag string
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
// afresh when the selection is another member than it was kept for. Caller
// must hold s.access.
func (s *MutableAutoSelect) trialsLocked(network string) *trialState {
	t := &s.trials[streakNetFor(network)]
	if sticky := loadString(s.stickySlot(network)); t.tag != sticky {
		*t = trialState{tag: sticky}
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
	sticky := loadString(s.stickySlot(network))
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
	trial.trialFor = sticky
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
	return (s.outageSuspectedLocked() || s.probesCantDecideLocked(network, sticky)) && t.setupOK &&
		now.Sub(t.unansweredAt) <= probeFreshnessWindow && now.Sub(t.lastAt) >= spacing
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
// trialFor and generation is current, or nil. Caller must hold s.access.
func (s *MutableAutoSelect) trialForLocked(network, trialFor string, generation uint64) *trialState {
	if t := s.trialsLocked(network); t.tag == trialFor && s.matchesGenerationLocked(generation) {
		return t
	}
	return nil
}

// noteSelectionFailedLocally records that the device failed a conn on
// network through tag, set up on network generation generation, itself: if
// tag is the selection, that stops trials until its next setup succeeds.
func (s *MutableAutoSelect) noteSelectionFailedLocally(tag, network string, generation uint64) {
	s.access.Lock()
	defer s.access.Unlock()
	if t := s.trialsLocked(network); t.tag == tag && s.matchesGenerationLocked(generation) {
		t.setupOK = false
	}
}

// noteSelectionSetup records whether a setup on tag, network's selection,
// on network generation generation, succeeded.
func (s *MutableAutoSelect) noteSelectionSetup(network, tag string, generation uint64, ok bool) {
	s.access.Lock()
	defer s.access.Unlock()
	if t := s.trialsLocked(network); t.tag == tag && s.matchesGenerationLocked(generation) {
		t.setupOK = ok
	}
}

// noteTrialAnswered acts on tag's trial conn on network, made for the
// selection trialFor, answering while the selection's traffic went
// unanswered within probeFreshnessWindow: tag serves while the selection
// doesn't, so the selection moves to tag. After a failed trial the answer
// may only mean the network is back, so the move waits for the selection to
// leave a conn opened after the answer unanswered.
func (s *MutableAutoSelect) noteTrialAnswered(tag, network string, generation uint64, trialFor string) {
	s.access.Lock()
	defer s.access.Unlock()
	t := s.trialForLocked(network, trialFor, generation)
	if t == nil || t.tag == "" || t.tag == tag || t.unansweredAt.IsZero() || time.Since(t.unansweredAt) > probeFreshnessWindow {
		return
	}
	if _, member := s.members.Load(tag); !member {
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

// noteTrialFailed records that a trial on network through tag, made for the
// selection trialFor, failed: a pending move to tag is dropped.
func (s *MutableAutoSelect) noteTrialFailed(tag, network, trialFor string, generation uint64) {
	s.access.Lock()
	defer s.access.Unlock()
	if t := s.trialForLocked(network, trialFor, generation); t != nil {
		t.failures++
		if t.pending == tag {
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
		s.closeStuckUDPToLocked(tag, dest)
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
	s.stickySlot(network).Store(tag)
	// Its trial's setup succeeded, and an app may keep using that handle.
	s.trialsLocked(network).setupOK = true
	s.logger.Info(network, " switch: ", prev, " -> ", tag, " (trial answered)")
	s.closeStuckOnReleaseLocked(network, prev)
	if network == N.NetworkUDP {
		s.inheritUDPDestinationsLocked(prev, tag)
	}
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
		sticky := loadString(&s.stickyTag.tcp)
		var tags []string
		if sticky != "" {
			tags = append(tags, sticky)
		}
		pool := s.replacementPoolLocked(time.Now(), N.NetworkTCP, sticky)
		for i := range min(len(pool), maxReplacementProbes) {
			tags = append(tags, pool[(s.recheckNext+i)%len(pool)].tag)
		}
		s.recheckNext += maxReplacementProbes
		if len(tags) > 0 {
			s.requestProbesLocked(tags, probeForced, false)
			s.dispatchProbesLocked()
		}
		s.armOutageRecheckLocked()
	})
}

// probeSelectionAfterOutageLocked probes the TCP selection again once
// passed's pass, completed at at, ended a suspected outage: the selection's
// own failures until then may have been the outage's, or its own. Caller
// must hold s.access.
func (s *MutableAutoSelect) probeSelectionAfterOutageLocked(passed string, at time.Time) {
	sticky := loadString(&s.stickyTag.tcp)
	if sticky == "" || sticky == passed {
		return
	}
	s.afterOutage.tag, s.afterOutage.passed, s.afterOutage.at = sticky, passed, at
	s.requestProbesLocked([]string{sticky}, probeForced, false)
	s.dispatchProbesLocked()
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
	if !outcome.success {
		s.trySwitchAfterConfirmedFailureLocked(tag, passed, at)
	}
}

// timeSpan is the times from from up to to.
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
		s.outageSince, s.outageEndedAt = time.Time{}, time.Now()
		s.pastOutages = append(s.pastOutages, timeSpan{s.outageEvidence, s.outageEndedAt})
		if len(s.pastOutages) > maxPastOutages {
			s.pastOutages = s.pastOutages[1:]
		}
		if s.outageRecheck != nil {
			s.outageRecheck.Stop()
		}
		s.logger.Info("member reachable again: confirmations resume")
	}
}

// outageEvidenceLocked reports whether a failure on a conn opened at opened
// says more about an outage of the device's own network than about its
// member: an outage is suspected, or the conn was opened before the latest
// one ended. Such failures aren't counted. Caller must hold s.access.
func (s *MutableAutoSelect) outageEvidenceLocked(opened time.Time) bool {
	return s.outageSuspectedLocked() || (!opened.IsZero() && opened.Before(s.outageEndedAt))
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
	out, member := s.members.Load(tag)
	// The attempt being accepted is still in flight but finalized.
	attempt := s.inFlightProbes[out]
	return member && attempt != nil && !attempt.finalized
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
// that has no recovery gate, or "" if there is none. A soft-demoted member is
// returned only once every clean one has a probe outcome since since. Caller
// must hold s.access.
func (s *MutableAutoSelect) findProbedReplacementLocked(now time.Time, network, excludeTag string, since time.Time) string {
	cleanUndecided := false
	for _, c := range s.replacementPoolLocked(now, network, excludeTag) {
		if c.demote == demoteClean {
			state := s.probeMembers[c.tag]
			cleanUndecided = cleanUndecided || state == nil || state.outcome == nil || state.outcome.completed.Before(since)
		} else if cleanUndecided {
			break
		}
		if s.eligibleReplacementLocked(now, c.tag) && s.passedSinceLocked(c.tag, since) {
			return c.tag
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
