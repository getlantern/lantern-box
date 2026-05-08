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

const maxDialRetries = 3

var (
	_ adapter.MutableOutboundGroup = (*MutableURLTest)(nil)
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
		nfact := lLog.NewFactory(slogger.SlogHandler().WithAttrs([]slog.Attr{slog.String("urltest_group", tag)}))
		log = nfact.Logger()
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
	return s.group.tags
}

// Add adds the given outbound tags to the group and returns the number of outbounds added. If an
// outbound tag already exists, it will be ignored.
func (s *MutableURLTest) Add(tags ...string) (n int, err error) {
	return s.group.Add(tags)
}

// Remove removes the given outbound tags from the group and returns the number of outbounds removed.
func (s *MutableURLTest) Remove(tags ...string) (n int, err error) {
	return s.group.Remove(tags)
}

// SetURLOverrides replaces the per-outbound URL override map used during URL testing.
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
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
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
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
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
	for n := range maxDialRetries {
		outbound, err := s.selectOutbound(network, tried)
		if err != nil {
			if !errors.Is(err, errNetworkNotSupported) && lastErr != nil {
				err = fmt.Errorf("%w: %w", ErrAllOutboundsFailed, lastErr)
			}
			span.RecordError(err)
			return zero, "", err
		}
		tag := realTag(outbound)
		conn, err := dial(outbound)
		if err == nil {
			return conn, tag, nil
		}
		s.logger.ErrorContext(ctx, err)
		span.AddEvent(failEvent, trace.WithAttributes(
			attribute.String("outbound", tag),
			attribute.Int("attempt", n+1),
			attribute.String("error", err.Error()),
		))
		s.group.markFailedAndReselect(outbound)
		lastErr = err
		if ctx.Err() != nil {
			span.RecordError(ctx.Err())
			return zero, "", ctx.Err()
		}
		tried[tag] = true
	}
	err := fmt.Errorf("%w: %w", ErrAllOutboundsFailed, lastErr)
	span.RecordError(err)
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
	if outbound == nil && len(excluded) == 0 && len(s.group.tags) > 0 {
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
	ctx                 context.Context
	outboundMgr         A.OutboundManager
	pauseMgr            pause.Manager
	logger              log.Logger
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
	started             bool
	isAlive             bool
	idleTimer           *time.Timer
	lastActive          common.TypedValue[time.Time]
	pauseC              chan struct{}
	cancel              context.CancelFunc
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
		tags:         tags,
		url:          link,
		urlOverrides: urlOverrides,
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
		return 0, errors.New("group is closed")
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
		g.outbounds.Store(tag, outbound)
		g.tags = append(g.tags, tag)
		n++
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("%d outbounds not found: %v", len(missing), missing)
	}
	return n, nil
}

func (g *urlTestGroup) SetURLOverrides(overrides map[string]string) {
	g.access.Lock()
	defer g.access.Unlock()
	g.urlOverrides = maps.Clone(overrides)
	// Clear URL test history for overridden tags so the next test cycle
	// re-tests them with the new callback URLs. Without this, outbounds
	// tested recently (within the configured interval) with OLD URLs would be
	// skipped, and the new bandit probe callbacks would never fire.
	for tag := range overrides {
		g.history.DeleteURLTestHistory(tag)
	}
}

func (g *urlTestGroup) Remove(tags []string) (n int, err error) {
	g.access.Lock()
	defer g.access.Unlock()

	if g.isClosed() {
		return 0, errors.New("group is closed")
	}
	if len(g.tags) == 0 {
		return 0, nil
	}

	for _, tag := range tags {
		if _, exists := g.outbounds.Load(tag); !exists {
			continue
		}
		g.outbounds.Delete(tag)
		g.history.DeleteURLTestHistory(tag)
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
	g.updateSelected()
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
	if len(g.tags) == 0 {
		return result, nil
	}
	g.logger.Trace("checking outbounds...")
	defer g.checking.Store(false)
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

// markFailedAndReselect drops the URL-test history for outbound and recomputes
// the selection.
func (g *urlTestGroup) markFailedAndReselect(outbound A.Outbound) {
	g.history.DeleteURLTestHistory(realTag(outbound))
	g.updateSelected()
}

func (g *urlTestGroup) updateSelected() {
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
	if current != nil && !excluded[realTag(current)] {
		if history := g.history.LoadURLTestHistory(realTag(current)); history != nil {
			minOutbound = current
			minDelay = history.Delay
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
// group's default URL is used.
func (g *urlTestGroup) testURLForTag(tag string) string {
	if g.urlOverrides != nil {
		if override, ok := g.urlOverrides[tag]; ok {
			return override
		}
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
	instance, err := detour.DialContext(ctx, "tcp", M.ParseSocksaddrHostPortStr(hostname, port))
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
