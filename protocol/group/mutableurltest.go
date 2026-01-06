package group

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
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
	"github.com/sagernet/sing/common/metadata"
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

func RegisterMutableURLTest(registry *outbound.Registry) {
	outbound.Register[option.MutableURLTestOutboundOptions](registry, constant.TypeMutableURLTest, NewMutableURLTest)
}

var (
	_ adapter.MutableOutboundGroup = (*MutableURLTest)(nil)
	_ A.OutboundGroup              = (*MutableURLTest)(nil)
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
			ctx, outboundMgr, log, options.Outbounds, options.URL, interval, idleTimeout, options.Tolerance,
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

func (s *MutableURLTest) URLTest(ctx context.Context) (map[string]uint16, error) {
	return s.group.URLTest(ctx)
}

func (s *MutableURLTest) CheckOutbounds() {
	s.group.CheckOutbounds(true)
}

func (s *MutableURLTest) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableURLTest.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	s.group.keepAlive()
	outbound, err := s.selectOutbound(network)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	conn, err := outbound.DialContext(ctx, network, destination)
	if err != nil {
		s.logger.ErrorContext(ctx, err)
		s.group.history.DeleteURLTestHistory(outbound.Tag())
		span.RecordError(err)
		return nil, err
	}
	if taggedConn, ok := conn.(*adapter.TaggedConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedConn(conn, realTag(outbound)), nil
}

func (s *MutableURLTest) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableURLTest.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	s.group.keepAlive()
	outbound, err := s.selectOutbound("udp")
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	conn, err := outbound.ListenPacket(ctx, destination)
	if err != nil {
		s.logger.ErrorContext(ctx, err)
		s.group.history.DeleteURLTestHistory(outbound.Tag())
		span.RecordError(err)
		return nil, err
	}
	if taggedConn, ok := conn.(*adapter.TaggedPacketConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedPacketConn(conn, realTag(outbound)), nil
}

func (s *MutableURLTest) selectOutbound(network string) (A.Outbound, error) {
	var outbound A.Outbound
	switch network {
	case "tcp":
		outbound = s.group.selectedOutboundTCP.Load()
	case "udp":
		outbound = s.group.selectedOutboundUDP.Load()
	default:
		return nil, fmt.Errorf("network %s not supported", network)
	}
	if outbound == nil {
		outbound = s.group.pickBestOutbound(network, nil)
	}
	if outbound == nil {
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
	interval, idleTimeout time.Duration,
	tolerance uint16,
) *urlTestGroup {
	ctx, cancel := context.WithCancel(ctx)
	var history A.URLTestHistoryStorage
	if historyFromCtx := service.PtrFromContext[urltest.HistoryStorage](ctx); historyFromCtx != nil {
		history = historyFromCtx
	} else if clashServer := service.FromContext[A.ClashServer](ctx); clashServer != nil {
		history = clashServer.HistoryStorage()
	} else {
		history = urltest.NewHistoryStorage()
	}
	return &urlTestGroup{
		ctx:         ctx,
		outboundMgr: outboundMgr,
		logger:      logger,
		tags:        tags,
		url:         link,
		history:     history,
		interval:    interval,
		idleTimeout: idleTimeout,
		tolerance:   tolerance,
		cancel:      cancel,
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
		g.idleTimer.Reset(g.idleTimeout)
		return
	}
	g.pauseC = make(chan struct{}, 1)
	go g.checkLoop()
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
	g.idleTimer = time.NewTimer(g.idleTimeout)
	g.isAlive = true
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
	b, _ := batch.New(ctx, batch.WithConcurrencyNum[any](10))
	checked := make(map[string]bool)
	var resultAccess sync.Mutex
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
		b.Go(realTag, func() (any, error) {
			testCtx, cancel := context.WithTimeout(g.ctx, C.TCPTimeout)
			defer cancel()
			g.logger.Trace("checking outbound", "tag", realTag)
			t, err := urltest.URLTest(testCtx, g.url, p)
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

func (g *urlTestGroup) updateSelected() {
	if len(g.tags) == 0 {
		g.selectedOutboundTCP.Store(nil)
		g.selectedOutboundUDP.Store(nil)
		return
	}
	tcpOutbound := g.selectedOutboundTCP.Load()
	if outbound := g.pickBestOutbound("tcp", tcpOutbound); outbound != tcpOutbound {
		g.selectedOutboundTCP.Store(outbound)
	}

	udpOutbound := g.selectedOutboundUDP.Load()
	if outbound := g.pickBestOutbound("udp", udpOutbound); outbound != udpOutbound {
		g.selectedOutboundUDP.Store(outbound)
	}
}

func (g *urlTestGroup) pickBestOutbound(network string, current A.Outbound) A.Outbound {
	var (
		minDelay    uint16
		minOutbound A.Outbound
	)
	if current != nil {
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
		if rTag == "" {
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
	for _, outbound := range g.outbounds.Iter() {
		if slices.Contains(outbound.Network(), network) && realTag(outbound) != "" {
			return outbound
		}
	}
	return nil
}

func realTag(outbound A.Outbound) string {
	if group, isGroup := outbound.(A.OutboundGroup); isGroup {
		return group.Now()
	}
	return outbound.Tag()
}
