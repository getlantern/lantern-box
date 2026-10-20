package group

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/interrupt"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

const tracerName = "github.com/getlantern/lantern-box/protocol/group"

func RegisterMutableSelector(registry *outbound.Registry) {
	outbound.Register[option.MutableSelectorOutboundOptions](registry, constant.TypeMutableSelector, NewMutableSelector)
}

var (
	_ adapter.MutableOutboundGroup = (*MutableSelector)(nil)
	_ A.OutboundGroup              = (*MutableSelector)(nil)
	_ A.ConnectionHandlerEx        = (*MutableSelector)(nil)
	_ A.PacketConnectionHandlerEx  = (*MutableSelector)(nil)
)

type MutableSelector struct {
	outbound.Adapter
	ctx         context.Context
	outboundMgr A.OutboundManager
	connMgr     A.ConnectionManager
	logger      logger.ContextLogger
	tags        []string
	outbounds   map[string]A.Outbound
	mu          sync.Mutex
	// selected pairs the selection with the group tracking every conn dialed or
	// routed through it, so a conn never mixes one selection's member with
	// another's group.
	selected atomic.TypedValue[*selection]
}

type selection struct {
	outbound A.Outbound
	group    *interrupt.Group
}

var (
	errNoOutbound = errors.New("no outbound available")
	// errSelectionChanged fails a dial that finished after a reselect retired its
	// selection, so callers retry instead of using a conn that is being closed.
	errSelectionChanged = errors.New("selection changed during dial")
)

func (sel *selection) member() A.Outbound {
	if sel == nil {
		return nil
	}
	return sel.outbound
}

func (sel *selection) tag() string {
	if m := sel.member(); m != nil {
		return m.Tag()
	}
	return ""
}

func (sel *selection) network() []string {
	if m := sel.member(); m != nil {
		return m.Network()
	}
	return nil
}

// load returns the current selection, or errNoOutbound when nothing is selected.
func (s *MutableSelector) load() (*selection, error) {
	sel := s.selected.Load()
	if sel.member() == nil {
		return nil, errNoOutbound
	}
	return sel, nil
}

func NewMutableSelector(ctx context.Context, router A.Router, logger log.ContextLogger, tag string, options option.MutableSelectorOutboundOptions) (A.Outbound, error) {
	selector := &MutableSelector{
		Adapter:     outbound.NewAdapter(constant.TypeMutableSelector, tag, nil, nil),
		ctx:         ctx,
		outboundMgr: service.FromContext[A.OutboundManager](ctx),
		connMgr:     service.FromContext[A.ConnectionManager](ctx),
		logger:      logger,
		tags:        options.Outbounds,
		outbounds:   make(map[string]A.Outbound),
	}
	return selector, nil
}

func (s *MutableSelector) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tags) == 0 {
		return nil
	}

	for _, tag := range s.tags {
		outbound, found := s.outboundMgr.Outbound(tag)
		if !found {
			return fmt.Errorf("outbound %s not found", tag)
		}
		s.outbounds[tag] = outbound
	}
	// restore previously selected outbound from cache file if available
	if cacheFile := service.FromContext[A.CacheFile](s.ctx); cacheFile != nil {
		if selected := cacheFile.LoadSelected(s.Tag()); selected != "" {
			if outbound, found := s.outbounds[selected]; found {
				s.storeSelected(outbound)
				return nil
			}
		}
	}
	s.storeSelected(s.outbounds[s.tags[0]])
	return nil
}

func (s *MutableSelector) SelectOutbound(tag string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectOutbound(tag)
}

func (s *MutableSelector) selectOutbound(tag string) bool {
	outbound, found := s.outbounds[tag]
	if !found {
		return false
	}
	if s.selected.Load().member() == outbound {
		return true
	}
	s.storeSelected(outbound)
	if cacheFile := service.FromContext[A.CacheFile](s.ctx); cacheFile != nil {
		if err := cacheFile.StoreSelected(s.Tag(), tag); err != nil {
			s.logger.Error("store selected outbound: ", err)
		}
	}
	return true
}

// storeSelected retires the old group, then sets the selection with a fresh
// one. Retiring first means no conn can join the old group after the switch:
// dials refuse (errSelectionChanged) and late routed flows are closed. Retire
// closes the old conns in the background, off s.mu.
func (s *MutableSelector) storeSelected(outbound A.Outbound) {
	if prev := s.selected.Load(); prev != nil {
		prev.group.Retire()
	}
	s.selected.Store(&selection{outbound: outbound, group: interrupt.NewGroup()})
}

// routeContext has the connection manager track the flow in g.
func routeContext(ctx context.Context, g *interrupt.Group) context.Context {
	return interrupt.ContextWithGroup(interrupt.ContextWithIsExternalConnection(ctx), g)
}

func (s *MutableSelector) Network() []string {
	return s.selected.Load().network()
}

func (s *MutableSelector) Now() string {
	return s.selected.Load().tag()
}

func (s *MutableSelector) All() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags
}

// Add adds the given outbound tags to the group and returns the number of outbounds added. If an
// outbound tag already exists, it will be ignored.
func (s *MutableSelector) Add(tags ...string) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var missing []string
	for _, tag := range tags {
		if _, exists := s.outbounds[tag]; exists {
			continue
		}
		outbound, found := s.outboundMgr.Outbound(tag)
		if !found {
			missing = append(missing, tag)
			continue
		}
		s.outbounds[tag] = outbound
		s.tags = append(s.tags, tag)
		n++
	}
	// Recover from Remove emptying the group.
	if s.selected.Load().member() == nil && len(s.tags) > 0 {
		s.storeSelected(s.outbounds[s.tags[0]])
	}
	if len(missing) > 0 {
		return n, fmt.Errorf("%d outbounds not found: %v", len(missing), missing)
	}
	return n, nil
}

// Remove removes the given outbound tags from the group and returns the number of outbounds removed.
// A random outbound will be selected if the currently selected outbound is removed.
func (s *MutableSelector) Remove(tags ...string) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tags) == 0 {
		return 0, nil
	}

	for _, tag := range tags {
		if _, exists := s.outbounds[tag]; !exists {
			continue
		}
		delete(s.outbounds, tag)
		n++
	}
	s.tags = slices.Collect(maps.Keys(s.outbounds))
	if _, exists := s.outbounds[s.Now()]; !exists {
		if len(s.tags) == 0 {
			s.storeSelected(nil)
		} else {
			s.selectOutbound(s.tags[0])
		}
	}
	return
}

func (s *MutableSelector) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	sel, selErr := s.load()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", sel.network()),
		attribute.String("outbound", sel.tag()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	if selErr != nil {
		span.RecordError(selErr)
		return nil, selErr
	}

	conn, err := sel.outbound.DialContext(ctx, network, destination)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	wrapped, ok := sel.group.TryNewConn(conn, interrupt.IsExternalConnectionFromContext(ctx))
	if !ok {
		conn.Close()
		span.RecordError(errSelectionChanged)
		return nil, errSelectionChanged
	}
	conn = wrapped
	if taggedConn, ok := conn.(*adapter.TaggedConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedConn(conn, realTag(sel.outbound)), nil
}

func (s *MutableSelector) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	sel, selErr := s.load()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", sel.network()),
		attribute.String("outbound", sel.tag()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()
	if selErr != nil {
		span.RecordError(selErr)
		return nil, selErr
	}

	conn, err := sel.outbound.ListenPacket(ctx, destination)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	wrapped, ok := sel.group.TryNewPacketConn(conn, interrupt.IsExternalConnectionFromContext(ctx))
	if !ok {
		conn.Close()
		span.RecordError(errSelectionChanged)
		return nil, errSelectionChanged
	}
	conn = wrapped
	if taggedConn, ok := conn.(*adapter.TaggedPacketConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedPacketConn(conn, realTag(sel.outbound)), nil
}

func (s *MutableSelector) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose network.CloseHandlerFunc) {
	sel, selErr := s.load()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.NewConnectionEx", trace.WithAttributes(
		attribute.String("inbound", sel.tag()),
		attribute.String("outbound", metadata.Outbound),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	wrappedOnClose := func(it error) {
		span.RecordError(it)
		if onClose != nil {
			onClose(it)
		}
	}
	if selErr != nil {
		s.logger.ErrorContext(ctx, selErr)
		wrappedOnClose(selErr)
		return
	}
	// A flow still dialing when the selection changes is tracked into the
	// retired group, which closes it at once.
	ctx = routeContext(ctx, sel.group)
	if handler, isHandler := sel.outbound.(A.ConnectionHandlerEx); isHandler {
		handler.NewConnectionEx(ctx, conn, metadata, wrappedOnClose)
		return
	}
	s.connMgr.NewConnection(ctx, sel.outbound, conn, metadata, wrappedOnClose)
}

func (s *MutableSelector) NewPacketConnectionEx(ctx context.Context, conn network.PacketConn, metadata A.InboundContext, onClose network.CloseHandlerFunc) {
	sel, selErr := s.load()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.NewPacketConnectionEx", trace.WithAttributes(
		attribute.String("inbound", sel.tag()),
		attribute.String("outbound", metadata.Outbound),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	wrappedOnClose := func(it error) {
		span.RecordError(it)
		if onClose != nil {
			onClose(it)
		}
	}
	if selErr != nil {
		s.logger.ErrorContext(ctx, selErr)
		wrappedOnClose(selErr)
		return
	}
	// A flow still dialing when the selection changes is tracked into the
	// retired group, which closes it at once.
	ctx = routeContext(ctx, sel.group)
	if handler, isHandler := sel.outbound.(A.PacketConnectionHandlerEx); isHandler {
		handler.NewPacketConnectionEx(ctx, conn, metadata, wrappedOnClose)
		return
	}
	s.connMgr.NewPacketConnection(ctx, sel.outbound, conn, metadata, wrappedOnClose)
}
