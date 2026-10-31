package group

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/batch"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/ntp"
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

func RegisterMutableURLTest(registry *outbound.Registry) {
	outbound.Register[option.MutableURLTestOutboundOptions](registry, constant.TypeMutableURLTest, NewMutableURLTest)
}

// ErrAllOutboundsFailed is returned by [MutableURLTest.DialContext] and [MutableURLTest.ListenPacket]
// after every attempted outbound has failed.
var ErrAllOutboundsFailed = errors.New("all outbounds failed")

// maxOutboundAttempts caps the number of outbounds DialContext / ListenPacket
// will try in a single call. Bounds worst-case latency when many outbounds are
// down (~maxOutboundAttempts * TCPTimeout instead of unbounded).
const maxOutboundAttempts = 3

var (
	_ adapter.MutableOutboundGroup = (*MutableURLTest)(nil)
	_ adapter.MemberReplacer       = (*MutableURLTest)(nil)
	_ A.InterfaceUpdateListener    = (*MutableURLTest)(nil)
	_ A.ConnectionHandlerEx        = (*MutableURLTest)(nil)
	_ A.PacketConnectionHandlerEx  = (*MutableURLTest)(nil)
)

type MutableURLTest struct {
	outbound.Adapter
	ctx         context.Context
	outboundMgr A.OutboundManager
	connMgr     A.ConnectionManager
	logger      log.ContextLogger
	group       *urlTestGroup
}

func NewMutableURLTest(ctx context.Context, _ A.Router, logger log.ContextLogger, tag string, options option.MutableURLTestOutboundOptions) (A.Outbound, error) {
	interval := time.Duration(options.Interval)
	if interval == 0 {
		interval = C.DefaultURLTestInterval
	}
	idleTimeout := time.Duration(options.IdleTimeout)
	if idleTimeout == 0 {
		idleTimeout = C.DefaultURLTestIdleTimeout
	}
	if interval > idleTimeout {
		return nil, errors.New("interval must be less or equal than idle_timeout")
	}
	if options.Tolerance == 0 {
		options.Tolerance = 50
	}

	log := logger
	if slogger, ok := logger.(lLog.SLogger); ok {
		log = slogger.WithAttrs(slog.String("urltest_group", tag))
	}
	outboundMgr := service.FromContext[A.OutboundManager](ctx)
	outbound := &MutableURLTest{
		Adapter:     outbound.NewAdapter(constant.TypeMutableURLTest, tag, []string{"tcp", "udp"}, nil),
		ctx:         ctx,
		outboundMgr: outboundMgr,
		connMgr:     service.FromContext[A.ConnectionManager](ctx),
		logger:      logger,
		group: newURLTestGroup(
			ctx, outboundMgr, log, options.Outbounds, options.URL, options.URLOverrides, interval, idleTimeout, options.Tolerance,
		),
	}
	return outbound, nil
}

func (s *MutableURLTest) Start() error {
	return s.group.Start()
}

func (s *MutableURLTest) PostStart() error {
	s.group.PostStart()
	return nil
}

func (s *MutableURLTest) Close() error {
	return s.group.Close()
}

func (s *MutableURLTest) InterfaceUpdated() {
	s.logger.Info("interface updated, restarting URL tests")
	s.group.onInterfaceUpdated()
}

func (s *MutableURLTest) Now() string {
	if outbound := s.group.selectedOutboundTCP.Load(); outbound != nil {
		return outbound.Tag()
	}
	if outbound := s.group.selectedOutboundUDP.Load(); outbound != nil {
		return outbound.Tag()
	}
	return ""
}

func (s *MutableURLTest) All() []string {
	s.group.selection.Lock()
	defer s.group.selection.Unlock()
	return slices.Clone(s.group.tags)
}

// Add adds the given outbound tags to the group and returns the number of outbounds added. If an
// outbound tag already exists, it will be ignored.
func (s *MutableURLTest) Add(tags ...string) (n int, err error) {
	return s.group.Add(tags)
}

// Replace swaps in the outbound manager's current outbound for each given tag already in the group
// and returns the number of members replaced. Tags not in the group are ignored. See
// [urlTestGroup.Replace].
func (s *MutableURLTest) Replace(tags ...string) (n int, err error) {
	return s.group.Replace(tags)
}

// Remove removes the given outbound tags from the group and returns the number of outbounds removed.
func (s *MutableURLTest) Remove(tags ...string) (n int, err error) {
	return s.group.Remove(tags)
}

// SetURLOverrides merges overrides into the per-outbound URLs used during URL
// testing. An override lasts until its tag is removed from the group.
func (s *MutableURLTest) SetURLOverrides(overrides map[string]string) {
	s.group.SetURLOverrides(overrides)
}

func (s *MutableURLTest) URLTest(ctx context.Context) (map[string]uint16, error) {
	return s.group.URLTest(ctx)
}

func (s *MutableURLTest) CheckOutbounds() {
	s.group.CheckOutbounds(true)
}

func (s *MutableURLTest) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableURLTest.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", s.Network()),
		tagAttr("outbound", s.Now()),
		tagAttr("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	conn, tag, err := tryEachOutbound(ctx, s, network, "dial_failed",
		func(o A.Outbound) (net.Conn, error) { return o.DialContext(ctx, network, destination) },
	)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*adapter.TaggedConn); ok {
		return tc, nil
	}
	return adapter.NewTaggedConn(conn, tag), nil
}

func (s *MutableURLTest) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableURLTest.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		tagAttr("outbound", s.Now()),
		tagAttr("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	conn, tag, err := tryEachOutbound(ctx, s, "udp", "listen_failed",
		func(o A.Outbound) (net.PacketConn, error) { return o.ListenPacket(ctx, destination) },
	)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*adapter.TaggedPacketConn); ok {
		return tc, nil
	}
	return adapter.NewTaggedPacketConn(conn, tag), nil
}

// tryEachOutbound attempts to dial or listen with each candidate outbound for the given network,
// up to maxDialRetries times.
func tryEachOutbound[T any](
	ctx context.Context,
	s *MutableURLTest,
	network string,
	failEvent string,
	dial func(A.Outbound) (T, error),
) (T, string, error) {
	s.group.keepAlive()
	var (
		tried   = make(map[string]bool)
		lastErr error
		zero    T
	)
	span := trace.SpanFromContext(ctx)
	for n := range maxOutboundAttempts {
		outbound, err := s.selectOutbound(network, tried)
		if err != nil {
			if !errors.Is(err, errNetworkNotSupported) && lastErr != nil {
				err = fmt.Errorf("%w: %w", ErrAllOutboundsFailed, lastErr)
			}
			recordError(span, err)
			return zero, "", err
		}
		triedTag := realTag(outbound)
		conn, err := dial(outbound)
		if err == nil {
			// Re-evaluate post-attempt so the returned tag reflects the
			// nested group's current selection rather than the stale pre-dial
			// snapshot.
			return conn, realTag(outbound), nil
		}
		s.logger.DebugContext(ctx, "outbound attempt failed",
			"outbound", triedTag, "attempt", n+1, "error", err)
		span.AddEvent(failEvent, trace.WithAttributes(
			tagAttr("outbound", triedTag),
			attribute.Int("attempt", n+1),
			attribute.String("error", adapter.RedactHelperTags(err.Error())),
		))
		s.group.markFailedAndReselect(outbound)
		lastErr = err
		if ctx.Err() != nil {
			recordError(span, ctx.Err())
			return zero, "", ctx.Err()
		}
		tried[triedTag] = true
	}
	err := fmt.Errorf("%w: %w", ErrAllOutboundsFailed, lastErr)
	recordError(span, err)
	return zero, "", err
}

var errNetworkNotSupported = errors.New("network not supported")

// selectOutbound returns the best candidate outbound for network whose realTag
// is not in excludedTags. excludedTags may be nil.
func (s *MutableURLTest) selectOutbound(network string, excluded map[string]bool) (A.Outbound, error) {
	var outbound A.Outbound
	switch network {
	case "tcp":
		outbound = s.group.selectedOutboundTCP.Load()
	case "udp":
		outbound = s.group.selectedOutboundUDP.Load()
	default:
		return nil, fmt.Errorf("%w: %s", errNetworkNotSupported, network)
	}
	if outbound != nil {
		if rTag := realTag(outbound); rTag == "" || excluded[rTag] {
			outbound = nil
		}
	}
	if outbound == nil {
		outbound = s.group.pickBestOutbound(network, nil, excluded)
	}
	recoveryStarted := false
	if outbound == nil && len(excluded) == 0 && s.group.hasMembers() {
		recoveryStarted = true
		s.logger.Warn("no outbound available, starting async URL test for recovery")
		go s.group.CheckOutbounds(true)
	}
	if outbound == nil {
		if recoveryStarted {
			return nil, errors.New("no outbound available, recovery in progress")
		}
		return nil, errors.New("missing supported outbound")
	}
	return outbound, nil
}

func (s *MutableURLTest) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewConnection(ctx, s, conn, metadata, onClose)
}

func (s *MutableURLTest) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata A.InboundContext, onClose N.CloseHandlerFunc) {
	s.connMgr.NewPacketConnection(ctx, s, conn, metadata, onClose)
}

type urlTestGroup struct {
	ctx         context.Context
	outboundMgr A.OutboundManager
	pauseMgr    pause.Manager
	logger      log.Logger
	// tags is written under both access and selection, so either suffices to read it.
	tags                []string
	outbounds           isync.TypedMap[string, A.Outbound]
	url                 string
	urlOverrides        map[string]string
	interval            time.Duration
	tolerance           uint16
	idleTimeout         time.Duration
	history             A.URLTestHistoryStorage
	selectedOutboundTCP common.TypedValue[A.Outbound]
	selectedOutboundUDP common.TypedValue[A.Outbound]
	access              sync.Mutex
	checking            atomic.Bool
	// selection serializes computing and publishing the selection, and
	// storing probe results, with Replace swapping members.
	selection sync.Mutex
	// awaitingProbe maps a replaced member's tag to the new outbound while its
	// first probe is in flight, so selection keeps it despite having no history.
	awaitingProbe isync.TypedMap[string, A.Outbound]
	started       bool
	isAlive       bool
	idleTimer     *time.Timer
	lastActive    common.TypedValue[time.Time]
	pauseC        chan struct{}
	cancel        context.CancelFunc
}

func newURLTestGroup(
	ctx context.Context,
	outboundMgr A.OutboundManager,
	logger log.ContextLogger,
	tags []string,
	link string,
	urlOverrides map[string]string,
	interval, idleTimeout time.Duration,
	tolerance uint16,
) *urlTestGroup {
	ctx, cancel := context.WithCancel(ctx)
	var history A.URLTestHistoryStorage
	if historyFromCtx := service.FromContext[A.URLTestHistoryStorage](ctx); historyFromCtx != nil {
		history = historyFromCtx
	} else if clashServer := service.FromContext[A.ClashServer](ctx); clashServer != nil {
		history = clashServer.HistoryStorage()
	} else {
		history = urltest.NewHistoryStorage()
	}
	return &urlTestGroup{
		ctx:          ctx,
		outboundMgr:  outboundMgr,
		logger:       logger,
		tags:         slices.Clone(tags),
		url:          link,
		urlOverrides: maps.Clone(urlOverrides),
		history:      history,
		interval:     interval,
		idleTimeout:  idleTimeout,
		tolerance:    tolerance,
		cancel:       cancel,
	}
}

func (g *urlTestGroup) Start() error {
	g.access.Lock()
	defer g.access.Unlock()

	if len(g.tags) == 0 {
		return nil
	}

	for _, tag := range g.tags {
		outbound, found := g.outboundMgr.Outbound(tag)
		if !found {
			g.outbounds.Clear()
			return fmt.Errorf("outbound %s not found", tag)
		}
		g.outbounds.Store(tag, outbound)
	}

	g.pauseMgr = service.FromContext[pause.Manager](g.ctx)
	g.updateSelected()
	return nil
}

func (g *urlTestGroup) PostStart() {
	g.access.Lock()
	defer g.access.Unlock()
	g.started = true
	g.lastActive.Store(time.Now())
	go g.CheckOutbounds(true)
}

func (g *urlTestGroup) Close() error {
	if g.isClosed() {
		return nil
	}
	g.cancel()

	g.access.Lock()
	defer g.access.Unlock()
	if g.pauseC != nil {
		close(g.pauseC)
	}
	return nil
}

func (g *urlTestGroup) isClosed() bool {
	select {
	case <-g.ctx.Done():
		return true
	default:
		return false
	}
}

func (g *urlTestGroup) Add(tags []string) (n int, err error) {
	g.access.Lock()
	defer g.access.Unlock()

	if g.isClosed() {
		return 0, adapter.ErrGroupClosed
	}

	var missing []string
	for _, tag := range tags {
		if _, exists := g.outbounds.Load(tag); exists {
			continue
		}
		outbound, found := g.outboundMgr.Outbound(tag)
		if !found {
			missing = append(missing, tag)
			continue
		}
		g.selection.Lock()
		g.outbounds.Store(tag, outbound)
		g.tags = append(g.tags, tag)
		g.selection.Unlock()
		n++
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("%d outbounds not found: %v", len(missing), missing)
	}
	return n, nil
}

// Replace re-resolves each given member tag from the outbound manager and swaps the new outbound
// into the group in place, for an outbound recreated under the same tag. The tag never leaves the
// group, so the group never empties. A replaced member's URL-test history is dropped and, once the
// group has started, the new outbound is probed in the background, independently of any running
// test. If the replaced member was
// selected, the selection moves to the new outbound and stays there until that probe's result is
// evaluated by the normal selection logic. Tags not in the group are ignored; a member missing from
// the outbound manager keeps its current outbound and is reported in the error.
func (g *urlTestGroup) Replace(tags []string) (n int, err error) {
	g.access.Lock()
	defer g.access.Unlock()

	if g.isClosed() {
		return 0, adapter.ErrGroupClosed
	}

	g.selection.Lock()
	defer g.selection.Unlock()

	var missing []string
	for _, tag := range tags {
		old, exists := g.outbounds.Load(tag)
		if !exists {
			continue
		}
		outbound, found := g.outboundMgr.Outbound(tag)
		if !found {
			missing = append(missing, tag)
			continue
		}
		n++
		if outbound == old {
			continue
		}
		g.outbounds.Store(tag, outbound)
		g.history.DeleteURLTestHistory(tag)
		g.awaitingProbe.Delete(tag)
		// A nested group probes its own members.
		if _, isGroup := outbound.(A.OutboundGroup); !isGroup && g.started {
			g.awaitingProbe.Store(tag, outbound)
			go g.probeReplacement(tag, outbound, g.testURLForTagLocked(tag))
		}
		g.replaceSelected(&g.selectedOutboundTCP, "tcp", old, outbound)
		g.replaceSelected(&g.selectedOutboundUDP, "udp", old, outbound)
		g.logger.Debug("replaced outbound", "tag", tag)
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("%d outbounds not found: %v", len(missing), missing)
	}
	return n, nil
}

// replaceSelected moves selected from old to its replacement outbound, or, if the replacement
// doesn't support network, to the best other member that does. Called with selection held.
func (g *urlTestGroup) replaceSelected(selected *common.TypedValue[A.Outbound], network string, old, outbound A.Outbound) {
	if selected.Load() != old {
		return
	}
	if slices.Contains(outbound.Network(), network) {
		selected.Store(outbound)
		return
	}
	selected.Store(g.pickBestOutbound(network, nil, nil))
}

// isAwaitingProbe reports whether outbound is a replacement whose first probe is in flight.
func (g *urlTestGroup) isAwaitingProbe(outbound A.Outbound) bool {
	pending, ok := g.awaitingProbe.Load(outbound.Tag())
	return ok && pending == outbound
}

func (g *urlTestGroup) SetURLOverrides(overrides map[string]string) {
	g.access.Lock()
	defer g.access.Unlock()
	// Probes check their URL is current under selection when storing a result.
	g.selection.Lock()
	defer g.selection.Unlock()
	if g.urlOverrides == nil {
		g.urlOverrides = make(map[string]string, len(overrides))
	}
	for tag, u := range overrides {
		if old, ok := g.urlOverrides[tag]; ok && old == u {
			continue
		}
		g.urlOverrides[tag] = u
		// Otherwise a tag tested within the interval is skipped and never
		// probes its new URL.
		g.history.DeleteURLTestHistory(tag)
	}
}

func (g *urlTestGroup) Remove(tags []string) (n int, err error) {
	g.access.Lock()
	defer g.access.Unlock()

	if g.isClosed() {
		return 0, adapter.ErrGroupClosed
	}
	if len(g.tags) == 0 {
		return 0, nil
	}

	// A probe holds selection between checking its member is current and
	// storing its result, so it can't restore a removed member's history.
	g.selection.Lock()
	defer g.selection.Unlock()
	for _, tag := range tags {
		if _, exists := g.outbounds.Load(tag); !exists {
			continue
		}
		g.outbounds.Delete(tag)
		delete(g.urlOverrides, tag)
		g.history.DeleteURLTestHistory(tag)
		g.awaitingProbe.Delete(tag)
		n++
	}
	g.tags = g.tags[:0]
	for tag := range g.outbounds.Iter() {
		g.tags = append(g.tags, tag)
	}
	if len(g.tags) == 0 && g.isAlive {
		select {
		case g.pauseC <- struct{}{}:
		default:
		}
	}
	g.updateSelectedLocked()
	return
}

func (g *urlTestGroup) keepAlive() {
	g.access.Lock()
	defer g.access.Unlock()
	if !g.started || len(g.tags) == 0 {
		return
	}
	if g.isAlive {
		g.lastActive.Store(time.Now())
		resetTimer(g.idleTimer, g.idleTimeout)
		return
	}
	g.isAlive = true
	g.idleTimer = time.NewTimer(g.idleTimeout)
	g.pauseC = make(chan struct{}, 1)
	go g.checkLoop()
}

func (g *urlTestGroup) onInterfaceUpdated() {
	g.access.Lock()
	if !g.started || g.isClosed() || len(g.tags) == 0 {
		g.access.Unlock()
		return
	}
	g.lastActive.Store(time.Now())
	wasAlive := g.isAlive
	if wasAlive {
		resetTimer(g.idleTimer, g.idleTimeout)
	}
	g.access.Unlock()

	if !wasAlive {
		g.keepAlive()
	}
	go g.CheckOutbounds(true)
}

func (g *urlTestGroup) checkLoop() {
	if time.Since(g.lastActive.Load()) > g.interval {
		g.lastActive.Store(time.Now())
		g.CheckOutbounds(false)
	}
	g.access.Lock()
	ctx, cancel := context.WithCancel(g.ctx)
	ticker := time.NewTicker(g.interval)
	pauseCallback := pause.RegisterTicker(g.pauseMgr, ticker, g.interval, nil)
	// idleTimer and isAlive are already set by keepAlive() under the lock
	// before spawning this goroutine, preventing duplicate checkLoop starts
	// and ensuring idleTimer is non-nil for concurrent keepAlive() callers.
	g.access.Unlock()

	defer func() {
		cancel()
		g.access.Lock()
		g.pauseMgr.UnregisterCallback(pauseCallback)
		g.idleTimer.Stop()
		g.isAlive = false
		g.access.Unlock()
	}()
	for {
		select {
		case <-g.pauseC:
			return
		case <-g.idleTimer.C:
			return
		case <-ctx.Done():
			g.logger.Warn("context canceled")
			return
		case <-ticker.C:
			go g.urlTest(ctx, false)
		}
	}
}

func (g *urlTestGroup) CheckOutbounds(force bool) {
	_, _ = g.urlTest(g.ctx, force)
}

func (g *urlTestGroup) URLTest(ctx context.Context) (map[string]uint16, error) {
	return g.urlTest(ctx, false)
}

func (g *urlTestGroup) urlTest(ctx context.Context, force bool) (map[string]uint16, error) {
	result := make(map[string]uint16)
	if g.checking.Swap(true) {
		return result, nil
	}
	defer g.checking.Store(false)
	if !g.hasMembers() {
		return result, nil
	}
	g.logger.Trace("checking outbounds...")
	b, _ := batch.New(ctx, batch.WithConcurrencyNum[any](6))
	checked := make(map[string]bool)
	var resultAccess sync.Mutex
	submitTime := time.Now() // track when outbounds were queued for delay reporting
	for tag, outbound := range g.outbounds.Iter() {
		// if outbound is an urltest group, start its own url test and skip
		if testGroup, isURLTestGroup := outbound.(A.URLTestGroup); isURLTestGroup {
			go testGroup.URLTest(ctx)
			continue
		}
		realTag := realTag(outbound) // gets the selected outbound if it's a group
		if realTag == "" {
			g.logger.Trace("skipping outbound", "tag", tag, "reason", "empty real tag")
			continue
		}
		if checked[realTag] {
			continue
		}
		history := g.history.LoadURLTestHistory(realTag)
		if !force && history != nil && time.Since(history.Time) < g.interval {
			continue
		}
		checked[realTag] = true
		p, loaded := g.outboundMgr.Outbound(realTag)
		if !loaded {
			g.logger.Trace("skipping outbound", "tag", realTag, "reason", "not found")
			continue
		}
		testURL := g.testURLForTag(tag)
		baseURL := testURL
		member := outbound
		b.Go(realTag, func() (any, error) {
			// Compute how long this outbound waited in the worker pool queue.
			// Append as &cd=<ms> so the server can subtract scheduling overhead
			// from the callback latency to get true proxy roundtrip time.
			queueDelay := time.Since(submitTime)
			if queueDelay > 5*time.Millisecond {
				testURL = appendClientDelay(testURL, queueDelay)
			}
			testCtx, cancel := context.WithTimeout(ctx, C.TCPTimeout)
			defer cancel()
			g.logger.Trace("checking outbound", "tag", realTag)
			t, err := urlTestGET(testCtx, testURL, p)
			g.selection.Lock()
			defer g.selection.Unlock()
			// A member replaced during the test has had its history reset
			// and a new test queued; this result describes the old instance.
			if current, _ := g.outbounds.Load(tag); current != member {
				g.logger.Debug("dropping result for replaced outbound", "tag", realTag)
				return nil, nil
			}
			// SetURLOverrides reset the history for a new URL; the next test uses it.
			if g.testURLForTagLocked(tag) != baseURL {
				g.logger.Debug("dropping result for overridden URL", "tag", realTag)
				return nil, nil
			}
			if err != nil {
				g.logger.Debug("outbound unavailable", "tag", realTag, "error", err)
				g.history.DeleteURLTestHistory(realTag)
				return nil, nil
			}
			g.logger.Debug("outbound available", "tag", realTag, "delay_ms", t)
			g.history.StoreURLTestHistory(realTag, &A.URLTestHistory{
				Time:  time.Now(),
				Delay: t,
			})
			resultAccess.Lock()
			result[tag] = t
			resultAccess.Unlock()
			return nil, nil
		})
	}
	b.Wait()
	g.updateSelected()
	return result, nil
}

// hasMembers reports whether the group has any member. Callers must not hold g.selection.
func (g *urlTestGroup) hasMembers() bool {
	g.selection.Lock()
	defer g.selection.Unlock()
	return len(g.tags) > 0
}

// probeReplacement URL-tests member, which Replace just installed under tag, independently of the
// group's regular tests. Its result is kept only if member is still the tag's member.
func (g *urlTestGroup) probeReplacement(tag string, member A.Outbound, testURL string) {
	ctx, cancel := context.WithTimeout(g.ctx, C.TCPTimeout)
	defer cancel()
	t, err := urlTestGET(ctx, testURL, member)
	g.selection.Lock()
	if current, _ := g.outbounds.Load(tag); current != member {
		g.selection.Unlock()
		g.logger.Debug("dropping result for replaced outbound", "tag", tag)
		return
	}
	if currentURL := g.testURLForTagLocked(tag); currentURL != testURL {
		// The member stays awaiting its probe; probe it again with the new URL.
		g.selection.Unlock()
		g.logger.Debug("reprobing replaced outbound with overridden URL", "tag", tag)
		go g.probeReplacement(tag, member, currentURL)
		return
	}
	g.awaitingProbe.CompareAndDelete(tag, member)
	if err != nil {
		g.logger.Debug("replaced outbound unavailable", "tag", tag, "error", err)
		g.history.DeleteURLTestHistory(tag)
	} else {
		g.logger.Debug("replaced outbound available", "tag", tag, "delay_ms", t)
		g.history.StoreURLTestHistory(tag, &A.URLTestHistory{Time: time.Now(), Delay: t})
	}
	g.selection.Unlock()
	g.updateSelected()
}

// markFailedAndReselect drops the URL-test history for outbound and recomputes
// the selection.
func (g *urlTestGroup) markFailedAndReselect(outbound A.Outbound) {
	g.awaitingProbe.CompareAndDelete(outbound.Tag(), outbound)
	g.history.DeleteURLTestHistory(realTag(outbound))
	g.updateSelected()
}

func (g *urlTestGroup) updateSelected() {
	g.selection.Lock()
	defer g.selection.Unlock()
	g.updateSelectedLocked()
}

func (g *urlTestGroup) updateSelectedLocked() {
	if len(g.tags) == 0 {
		g.selectedOutboundTCP.Store(nil)
		g.selectedOutboundUDP.Store(nil)
		return
	}
	tcpOutbound := g.selectedOutboundTCP.Load()
	if outbound := g.pickBestOutbound("tcp", tcpOutbound, nil); outbound != tcpOutbound {
		g.selectedOutboundTCP.Store(outbound)
	}

	udpOutbound := g.selectedOutboundUDP.Load()
	if outbound := g.pickBestOutbound("udp", udpOutbound, nil); outbound != udpOutbound {
		g.selectedOutboundUDP.Store(outbound)
	}
}

// pickBestOutbound returns the lowest-latency outbound supporting network,
// preferring current within tolerance. Outbounds whose realTag is in excluded
// are skipped (excluded may be nil). Falls back to any non-current candidate
// when no outbound has URL-test history.
func (g *urlTestGroup) pickBestOutbound(network string, current A.Outbound, excluded map[string]bool) A.Outbound {
	var (
		minDelay    uint16
		minOutbound A.Outbound
	)
	if current != nil && !excluded[realTag(current)] && slices.Contains(current.Network(), network) {
		if history := g.history.LoadURLTestHistory(realTag(current)); history != nil {
			minOutbound = current
			minDelay = history.Delay
		} else if g.isAwaitingProbe(current) {
			// A replacement keeps its selection until its first probe lands.
			return current
		}
	}
	for _, outbound := range g.outbounds.Iter() {
		if !slices.Contains(outbound.Network(), network) {
			continue
		}
		rTag := realTag(outbound)
		if rTag == "" || excluded[rTag] {
			continue
		}
		history := g.history.LoadURLTestHistory(rTag)
		if history == nil {
			continue
		}
		if minDelay == 0 || minDelay > history.Delay+g.tolerance {
			minDelay = history.Delay
			minOutbound = outbound
		}
	}
	if minOutbound != nil {
		return minOutbound
	}
	// Reaching this loop means no candidate has URL-test history. Skip current:
	// any other choice is no worse, and avoids re-picking an outbound just
	// deleted by markFailedAndReselect.
	for _, outbound := range g.outbounds.Iter() {
		if outbound == current {
			continue
		}
		rTag := realTag(outbound)
		if rTag == "" || excluded[rTag] {
			continue
		}
		if slices.Contains(outbound.Network(), network) {
			return outbound
		}
	}
	return nil
}

// testURLForTag returns the URL to use when testing the given outbound tag.
// If the tag has a per-outbound override, that URL is returned; otherwise the
// group's default URL is used. Callers must not hold g.selection.
func (g *urlTestGroup) testURLForTag(tag string) string {
	g.selection.Lock()
	defer g.selection.Unlock()
	return g.testURLForTagLocked(tag)
}

// testURLForTagLocked is testURLForTag for callers holding g.selection or
// g.access; the overrides change only under both.
func (g *urlTestGroup) testURLForTagLocked(tag string) string {
	if override, ok := g.urlOverrides[tag]; ok {
		return override
	}
	return g.url
}

// resetTimer safely resets a timer by stopping it and draining the channel
// before resetting. This prevents a stale fire from causing checkLoop to exit
// immediately after the reset.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func realTag(outbound A.Outbound) string {
	if group, isGroup := outbound.(A.OutboundGroup); isGroup {
		return group.Now()
	}
	return outbound.Tag()
}

// urlTestGET is a replacement for urltest.URLTest that sends a GET request
// instead of HEAD. This allows future use of the response body (e.g., to
// validate proxy behavior or carry data back from the test endpoint).
func urlTestGET(ctx context.Context, link string, detour N.Dialer) (uint16, error) {
	if link == "" {
		link = "https://www.gstatic.com/generate_204"
	}
	linkURL, err := url.Parse(link)
	if err != nil {
		return 0, err
	}
	hostname := linkURL.Hostname()
	port := linkURL.Port()
	if port == "" {
		switch linkURL.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}

	start := time.Now()
	instance, err := detour.DialContext(adapter.ContextWithProbe(ctx), "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
	if err != nil {
		return 0, err
	}
	defer instance.Close()
	if earlyConn, isEarlyConn := common.Cast[N.EarlyConn](instance); isEarlyConn && earlyConn.NeedHandshake() {
		start = time.Now()
	}
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return 0, err
	}
	// Propagate embedded trace context so the bandit callback
	// appears in the same distributed trace as the config assignment.
	if tp := linkURL.Query().Get("tp"); tp != "" {
		req.Header.Set("traceparent", tp)
	}
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return instance, nil
			},
			TLSClientConfig: &tls.Config{
				Time:    ntp.TimeFuncFromContext(ctx),
				RootCAs: A.RootPoolFromContext(ctx),
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: C.TCPTimeout,
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return uint16(time.Since(start) / time.Millisecond), nil
}

// appendClientDelay adds a &cd=<ms> query parameter to the given URL.
func appendClientDelay(rawURL string, delay time.Duration) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("cd", strconv.FormatInt(delay.Milliseconds(), 10))
	u.RawQuery = q.Encode()
	return u.String()
}
