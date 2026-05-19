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

const probeConcurrency = 6

// MutableAutoSelect is the client-side server-selection group. It probes
// its members in parallel, picks the lowest-delay healthy candidate with
// switch-tolerance hysteresis, demotes servers whose probes pass but whose
// user traffic fails, watches connections for data-plane stalls, and
// emits an exhaustion signal when every candidate is blocked.
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

	// Last dial-time tag per network, for switch-tolerance hysteresis.
	// atomic.Value (not access-guarded) so Now() can read without taking
	// the access lock. A stale tag is harmless: the in-pool check in
	// applyStickiness drops it.
	stickyTag struct {
		tcp atomic.Value // string; "" when unset
		udp atomic.Value // string; "" when unset
	}

	// probeMu serializes probe cycles. Fire-and-forget callers
	// (runProbeCycle) TryLock; callers that need a deterministic outcome
	// (URLTest, runLadder) Lock so they observe the cycle they triggered.
	probeMu           sync.Mutex
	laddering         atomic.Bool
	exhaustionEmitted atomic.Bool
	exhaustionCh      chan struct{}

	// Unix-nano of the most recent non-empty data-plane Read/Write; 0
	// means no traffic observed yet. Drives the adaptive probe cadence.
	lastActive atomic.Int64

	started   bool
	closeOnce sync.Once
}

type mutableAutoSelectConfig struct {
	switchTolerance   time.Duration
	activeInterval    time.Duration
	idleInterval      time.Duration
	idleThreshold     time.Duration
	ladderFastRetry   time.Duration
	ladderTotalBudget time.Duration
	dataPlaneIdle     time.Duration
}

func resolveMutableAutoSelectOptions(o option.MutableAutoSelectOutboundOptions) (mutableAutoSelectConfig, historyParams) {
	cfg := mutableAutoSelectConfig{
		switchTolerance:   time.Duration(o.SwitchToleranceMs) * time.Millisecond,
		activeInterval:    time.Duration(o.BackgroundIntervalSeconds) * time.Second,
		idleInterval:      time.Duration(o.IdleIntervalSeconds) * time.Second,
		idleThreshold:     time.Duration(o.IdleThresholdSeconds) * time.Second,
		ladderFastRetry:   time.Duration(o.LadderFastRetryMs) * time.Millisecond,
		ladderTotalBudget: time.Duration(o.LadderTotalBudgetSeconds) * time.Second,
		dataPlaneIdle:     time.Duration(o.DataPlaneIdleSeconds) * time.Second,
	}
	if cfg.switchTolerance == 0 {
		cfg.switchTolerance = 50 * time.Millisecond
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
	if cfg.ladderFastRetry == 0 {
		cfg.ladderFastRetry = 1000 * time.Millisecond
	}
	if cfg.ladderTotalBudget == 0 {
		cfg.ladderTotalBudget = 10 * time.Second
	}
	if cfg.dataPlaneIdle == 0 {
		cfg.dataPlaneIdle = defaultDataPlaneIdle
	}

	hp := defaultHistoryParams()
	if o.HistoryRingSize > 0 {
		hp.ringMax = int(o.HistoryRingSize)
	}
	if o.SuccessRateWindowMinutes > 0 {
		hp.successRateWindow = time.Duration(o.SuccessRateWindowMinutes) * time.Minute
	}
	if o.DemoteSuccessRateBelow > 0 {
		hp.demoteSuccessRate = o.DemoteSuccessRateBelow
	}
	if o.DemoteMinOutcomes > 0 {
		hp.demoteMinOutcomes = o.DemoteMinOutcomes
	}
	if o.ConsecutiveFailureLimit > 0 {
		hp.consecutiveFailLimit = o.ConsecutiveFailureLimit
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

// hydrateHistoryLocked seeds the in-memory localHistory from the persisted
// snapshot. No-op when an in-memory entry already exists. Caller must hold
// s.access.
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
	s.histories[tag] = hydrateLocalHistory(snap, s.hist.ringMax)
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
		return 0, errors.New("group is closed")
	}
	var missing []string
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
		n++
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
		return 0, errors.New("group is closed")
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
		// Preserve registration order; rank() ties on first-seen in s.tags.
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

// SetURLOverrides replaces the per-member callback URL map. Members whose
// override changed (including removals) get their history dropped so the
// next probe cycle re-tests against the new URL.
func (s *MutableAutoSelect) SetURLOverrides(overrides map[string]string) {
	s.access.Lock()
	defer s.access.Unlock()
	old := s.urlOverrides
	s.urlOverrides = maps.Clone(overrides)
	for tag, v := range s.urlOverrides {
		if old[tag] != v {
			s.invalidateHistoryLocked(tag)
		}
	}
	for tag := range old {
		if _, kept := s.urlOverrides[tag]; !kept {
			s.invalidateHistoryLocked(tag)
		}
	}
}

// invalidateHistoryLocked drops both the in-memory ring and the persisted
// snapshot for tag. Caller must hold s.access.
func (s *MutableAutoSelect) invalidateHistoryLocked(tag string) {
	delete(s.histories, tag)
	if s.history != nil {
		s.history.Delete(tag)
	}
}

func (s *MutableAutoSelect) CheckOutbounds() {
	go s.runProbeCycle(s.ctx)
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
	if len(s.tags) == 0 {
		s.access.Unlock()
		return results, nil
	}
	for _, tag := range s.tags {
		if _, ok := s.members.Load(tag); ok {
			continue
		}
		o, found := s.outboundMgr.Outbound(tag)
		if !found {
			continue
		}
		s.members.Store(tag, o)
		// Hydrate before the first recordOutcome lands so the persisted
		// ring isn't clobbered by a one-entry write from an empty
		// in-memory localHistory.
		s.hydrateHistoryLocked(tag)
	}
	jobs := s.collectProbeJobsLocked()
	s.access.Unlock()

	s.probeAll(ctx, jobs, func(res probeResult) {
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
	if err != nil {
		s.logger.ErrorContext(ctx, err)
		// Attribute the failure to the outer (member) tag so rank and
		// runLadder's skip-step-1 check both see the bump. For nested
		// groups, the inner tag isn't a member of this group, so
		// recordUserFailure would drop on the member gate.
		s.recordUserFailure(outerTag)
		go s.runLadder(outerTag)
		span.RecordError(err)
		return nil, err
	}
	onStall, onRead := s.makeHooks(outerTag)
	wrapped := newDataPlaneStream(conn, s.cfg.dataPlaneIdle, onStall, s.bumpActive, onRead)
	return adapter.NewTaggedConn(wrapped, realTag(o)), nil
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
	if err != nil {
		s.logger.ErrorContext(ctx, err)
		s.recordUserFailure(outerTag)
		go s.runLadder(outerTag)
		span.RecordError(err)
		return nil, err
	}
	onStall, onRead := s.makeHooks(outerTag)
	wrapped := newDataPlanePacket(conn, s.cfg.dataPlaneIdle, onStall, s.bumpActive, onRead)
	return adapter.NewTaggedPacketConn(wrapped, realTag(o)), nil
}

func (s *MutableAutoSelect) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableAutoSelect) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewPacketConnection(ctx, s, conn, metadata, onClose)
}

// selectFor ranks every viable member, applies the three-tier demote
// fallback per network, and picks the winner with switch-tolerance
// hysteresis against the previous sticky tag.
func (s *MutableAutoSelect) selectFor(network string) (A.Outbound, error) {
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
	ranked := s.rank(time.Now(), time.Time{})
	pool := splitHealthyFor(ranked, network)
	if len(pool) == 0 {
		s.access.Unlock()
		s.CheckOutbounds()
		return nil, errors.New("no outbound available, recovery in progress")
	}
	winner := s.applyStickiness(network, slot, pool)
	prev := loadString(slot)
	if prev != winner.tag {
		slot.Store(winner.tag)
		if prev == "" {
			s.logger.Info(network, " select: ", winner.tag)
		}
	}
	s.access.Unlock()
	return winner.outbound, nil
}

// applyStickiness applies the spec's switch-tolerance hysteresis: keep
// the sticky tag if it's in pool and the rank winner doesn't beat it by
// at least switchTolerance. Caller must hold s.access.
func (s *MutableAutoSelect) applyStickiness(network string, slot *atomic.Value, pool []rankedCandidate) rankedCandidate {
	best := pool[0]
	sticky := loadString(slot)
	if sticky == "" || sticky == best.tag {
		return best
	}
	for _, c := range pool {
		if c.tag != sticky {
			continue
		}
		tolMs := uint64(s.cfg.switchTolerance / time.Millisecond)
		if uint64(best.delayMs)+tolMs <= uint64(c.delayMs) {
			s.logger.Info(network, " switch: ", c.tag, "(", c.delayMs, "ms) -> ", best.tag, "(", best.delayMs, "ms)")
			return best
		}
		return c
	}
	s.logger.Info(network, " switch: ", sticky, " -> ", best.tag, " (sticky not in pool)")
	return best
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
		h = newLocalHistory(s.hist.ringMax)
		s.histories[tag] = h
	}
	return h
}

func (s *MutableAutoSelect) peekHistoryLocked(tag string) (*localHistory, bool) {
	h, ok := s.histories[tag]
	return h, ok
}

// collectProbeJobsLocked snapshots the current member set into a slice of
// probe jobs. Excluded protocols (tor, unbounded) are skipped. Caller must
// hold s.access.
func (s *MutableAutoSelect) collectProbeJobsLocked() []probeJob {
	jobs := make([]probeJob, 0, len(s.tags))
	for _, tag := range s.tags {
		o, ok := s.members.Load(tag)
		if !ok {
			continue
		}
		beh := behaviorFor(o.Type())
		if beh.excludeFromPool {
			continue
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
// Ordering is load-bearing: rank's sort treats the integer value as the
// tie-break key, so real-seeded must come first and substituted last.
type candidateKind uint8

const (
	kindRealSeeded  candidateKind = iota // measured delay from a real probe
	kindUnknown                          // no in-memory data and no persisted seed
	kindSubstituted                      // protocol carries a substituteDelay (samizdat)
)

// demoteLevel ranks how cautious selection should be about a candidate.
// Ordering is load-bearing: rank sorts on the integer value, so
// demoteClean must compare < demoteSoft < demoteHard for the tiers to
// land in the right order.
type demoteLevel uint8

const (
	demoteClean demoteLevel = iota
	demoteSoft              // userFailures > 0 but demote() says no
	demoteHard              // demote() returned true
)

type rankedCandidate struct {
	outbound A.Outbound
	tag      string
	delayMs  uint32
	demote   demoteLevel
	kind     candidateKind
}

// splitHealthyFor restricts ranked to candidates supporting network,
// then returns the cleanest non-empty subset: clean if any exist, else
// soft-demoted, else the network-filtered slice (hard-demoted as last
// resort). Per-network so a soft-only UDP candidate isn't drowned out
// by a clean TCP-only peer.
func splitHealthyFor(ranked []rankedCandidate, network string) []rankedCandidate {
	var filtered, clean, soft []rankedCandidate
	for _, c := range ranked {
		if !slices.Contains(c.outbound.Network(), network) {
			continue
		}
		filtered = append(filtered, c)
		switch c.demote {
		case demoteClean:
			clean = append(clean, c)
		case demoteSoft:
			soft = append(soft, c)
		}
	}
	if len(clean) > 0 {
		return clean
	}
	if len(soft) > 0 {
		return soft
	}
	return filtered
}

// rank builds the candidate set for selection. A non-zero freshSince
// restricts to members with a recorded outcome at or after freshSince
// and drops those without a fresh success (rawDelay==0); the
// probe-cycle ranker uses this so the "is there a winner this cycle?"
// check can't see stale outcomes. Caller must hold s.access.
func (s *MutableAutoSelect) rank(now time.Time, freshSince time.Time) []rankedCandidate {
	out := make([]rankedCandidate, 0, len(s.tags))
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
			rawDelay  uint32
			hard      bool
			userFails uint32
			haveData  bool
		)
		if h, ok := s.peekHistoryLocked(tag); ok {
			entries, consec, fails := h.snapshot()
			if !freshSince.IsZero() && !hasOutcomeSince(entries, freshSince) {
				continue
			}
			rawDelay = lastSuccessDelay(entries)
			hard = demoted(entries, consec, fails, now, s.hist)
			userFails = fails
			haveData = true
		} else if freshSince.IsZero() && s.history != nil {
			// Cold member (added without going through hydrate). Fall back
			// to the persisted snapshot so recompute-style callers see
			// host-injected data. Probe-cycle callers (freshSince set) skip
			// this branch — they only count outcomes from this cycle.
			if seed := s.history.Load(tag); seed != nil {
				entries := localizeOutcomes(seed.Outcomes)
				rawDelay = lastSuccessDelay(entries)
				hard = demoted(entries, seed.ConsecutiveFailures, seed.UserFailures, now, s.hist)
				userFails = seed.UserFailures
				haveData = true
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
		level := demoteClean
		switch {
		case hard:
			level = demoteHard
		case userFails > 0:
			level = demoteSoft
		}
		out = append(out, rankedCandidate{outbound: o, tag: tag, delayMs: delay, kind: kind, demote: level})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.demote != b.demote {
			return a.demote < b.demote
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.delayMs < b.delayMs
	})
	return out
}

// runProbeCycle is the fire-and-forget entry point; it skips when another
// cycle is in flight. Callers that need a deterministic outcome acquire
// s.probeMu directly and call probeCycleInner.
func (s *MutableAutoSelect) runProbeCycle(ctx context.Context) {
	if !s.probeMu.TryLock() {
		return
	}
	defer s.probeMu.Unlock()
	s.probeCycleInner(ctx, time.Now())
}

// probeCycleInner probes every non-excluded member in parallel and
// returns the lowest-delay candidate that succeeded at or after
// cycleStart, or nil when no candidate succeeded. Caller must hold
// s.probeMu so concurrent cycles can't interleave recordOutcome calls.
func (s *MutableAutoSelect) probeCycleInner(ctx context.Context, cycleStart time.Time) A.Outbound {
	s.access.Lock()
	jobs := s.collectProbeJobsLocked()
	s.access.Unlock()
	if len(jobs) == 0 {
		return nil
	}
	s.probeAll(ctx, jobs, nil)

	s.access.Lock()
	defer s.access.Unlock()
	ranked := s.rank(time.Now(), cycleStart)
	if len(ranked) == 0 {
		return nil
	}
	return ranked[0].outbound
}

// mutateHistory applies fn to tag's history under s.access and persists
// the result when fn returns true. Drops silently when tag is no longer a
// member. Holds the lock through both the in-memory mutation and the
// persistence write so a concurrent Remove can't be undone by a late
// goroutine. fn is passed the timestamp used for both the entry and the
// persisted snapshot.
func (s *MutableAutoSelect) mutateHistory(tag string, fn func(*localHistory, time.Time) bool) {
	s.access.Lock()
	defer s.access.Unlock()
	if _, member := s.members.Load(tag); !member {
		return
	}
	h := s.historyForLocked(tag)
	now := time.Now()
	if !fn(h, now) || s.history == nil {
		return
	}
	s.history.Store(tag, h.toTagHistory(now))
}

func (s *MutableAutoSelect) recordUserFailure(tag string) {
	s.mutateHistory(tag, func(h *localHistory, _ time.Time) bool {
		h.bumpUserFailures()
		return true
	})
}

func (s *MutableAutoSelect) recordOutcome(tag string, o localOutcome, delayMs uint32) {
	s.mutateHistory(tag, func(h *localHistory, now time.Time) bool {
		h.append(o, delayMs, now)
		return true
	})
}

// runLadder runs the two-step reconnection ladder (fast retry of target,
// then a full parallel re-probe) and emits the exhaustion signal if
// neither step produced a working candidate. Concurrent invocations
// collapse via the laddering CAS guard. An empty target falls back to
// the most recent sticky tag (TCP preferred, UDP secondary).
//
// Step 1 is skipped when target has any user-failure evidence: probe
// URLs can still pass while real traffic doesn't (a common censorship
// shape), and step 1 would just launder those failures.
func (s *MutableAutoSelect) runLadder(target string) {
	if s.isClosed() {
		return
	}
	if !s.laddering.CompareAndSwap(false, true) {
		return
	}
	defer s.laddering.Store(false)
	s.exhaustionEmitted.Store(false)
	deadline := time.Now().Add(s.cfg.ladderTotalBudget)

	var current A.Outbound
	if target != "" {
		if o, ok := s.members.Load(target); ok {
			current = o
		}
	}
	if current == nil {
		for _, t := range [2]string{
			loadString(&s.stickyTag.tcp),
			loadString(&s.stickyTag.udp),
		} {
			if t == "" {
				continue
			}
			if o, ok := s.members.Load(t); ok {
				current = o
				break
			}
		}
	}
	skipStep1 := false
	if current != nil {
		s.access.Lock()
		if h, ok := s.peekHistoryLocked(current.Tag()); ok && h.userFailureCount() > 0 {
			skipStep1 = true
		}
		s.access.Unlock()
	}
	if current != nil && !skipStep1 {
		s.logger.Info("ladder step 1: fast retry of ", current.Tag())
		fastCtx, cancel := context.WithTimeout(s.ctx, s.cfg.ladderFastRetry)
		defer cancel()
		s.access.Lock()
		probeURL := s.probeURLForLocked(current.Tag())
		beh := behaviorFor(current.Type())
		s.access.Unlock()
		res := runProbe(fastCtx, current, probeURL, beh)
		s.recordOutcome(res.tag, res.outcome, res.delayMs)
		if res.outcome == outcomeSuccess {
			s.logger.Info("ladder step 1 succeeded for ", current.Tag())
			return
		}
	} else if skipStep1 {
		s.logger.Info("ladder skipping step 1: ", current.Tag(),
			" has a non-zero user-failure count; jumping straight to step 2")
	}

	s.logger.Info("ladder step 2: full re-probe")
	// Lock (not TryLock) so the exhaustion gate keys off this call's
	// cycle, never a stale bg cycle. Compute the budget after the wait
	// so serialization queueing doesn't eat probing time.
	s.probeMu.Lock()
	remaining := time.Until(deadline)
	if minStep2 := s.cfg.ladderTotalBudget / 2; remaining < minStep2 {
		remaining = minStep2
	}
	fullCtx, cancel := context.WithTimeout(s.ctx, remaining)
	defer cancel()
	winner := s.probeCycleInner(fullCtx, time.Now())
	s.probeMu.Unlock()
	if s.isClosed() {
		return
	}
	if winner == nil {
		s.logger.Warn("ladder exhausted: no candidate succeeded")
		s.emitExhaustion()
		return
	}
	s.logger.Info("ladder step 2 found winner: ", winner.Tag())
}

func (s *MutableAutoSelect) emitExhaustion() {
	if !s.exhaustionEmitted.CompareAndSwap(false, true) {
		return
	}
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

// runBackgroundLoop drives the low-priority probe cycle that keeps
// alternative members warm. The ticker is registered with the pause
// manager so probes are suppressed while the device is paused; the
// adaptive cadence reasserts on every tick via nextProbeInterval.
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
