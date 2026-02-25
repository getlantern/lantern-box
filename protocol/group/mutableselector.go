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
	ctx            context.Context
	outboundMgr    A.OutboundManager
	connMgr        A.ConnectionManager
	logger         logger.ContextLogger
	tags           []string
	outbounds      map[string]A.Outbound
	selected       atomic.TypedValue[A.Outbound]
	mu             sync.Mutex
	interruptGroup *interrupt.Group
}

func NewMutableSelector(ctx context.Context, router A.Router, logger log.ContextLogger, tag string, options option.MutableSelectorOutboundOptions) (A.Outbound, error) {
	selector := &MutableSelector{
		Adapter:        outbound.NewAdapter(constant.TypeMutableSelector, tag, nil, nil),
		ctx:            ctx,
		outboundMgr:    service.FromContext[A.OutboundManager](ctx),
		connMgr:        service.FromContext[A.ConnectionManager](ctx),
		logger:         logger,
		tags:           options.Outbounds,
		outbounds:      make(map[string]A.Outbound),
		interruptGroup: interrupt.NewGroup(),
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
				s.selected.Store(outbound)
				return nil
			}
		}
	}
	s.selected.Store(s.outbounds[s.tags[0]])
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
	if s.selected.Swap(outbound) == outbound {
		return true
	}
	if cacheFile := service.FromContext[A.CacheFile](s.ctx); cacheFile != nil {
		if err := cacheFile.StoreSelected(s.Tag(), tag); err != nil {
			s.logger.Error("store selected outbound: ", err)
		}
	}
	s.interruptGroup.Interrupt(true)
	return true
}

func (s *MutableSelector) Network() []string {
	outbound := s.selected.Load()
	if outbound == nil {
		return nil
	}
	return outbound.Network()
}

func (s *MutableSelector) Now() string {
	outbound := s.selected.Load()
	if outbound == nil {
		return ""
	}
	return outbound.Tag()
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
			s.selected.Store(nil)
		} else {
			s.selectOutbound(s.tags[0])
		}
	}
	return
}

func (s *MutableSelector) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.DialContext", trace.WithAttributes(
		attribute.String("network", network),
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	outbound := s.selected.Load()
	if outbound == nil {
		err := errors.New("no outbound available")
		span.RecordError(err)
		return nil, err
	}

	conn, err := outbound.DialContext(ctx, network, destination)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	conn = s.interruptGroup.NewConn(conn, interrupt.IsExternalConnectionFromContext(ctx))
	if taggedConn, ok := conn.(*adapter.TaggedConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedConn(conn, realTag(outbound)), nil
}

func (s *MutableSelector) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.ListenPacket", trace.WithAttributes(
		attribute.StringSlice("supported_network_options", s.Network()),
		attribute.String("outbound", s.Now()),
		attribute.String("tag", s.Tag()),
		attribute.String("type", s.Type()),
	))
	defer span.End()

	outbound := s.selected.Load()
	if outbound == nil {
		err := errors.New("no outbound available")
		span.RecordError(err)
		return nil, err
	}

	conn, err := outbound.ListenPacket(ctx, destination)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	conn = s.interruptGroup.NewPacketConn(conn, interrupt.IsExternalConnectionFromContext(ctx))
	if taggedConn, ok := conn.(*adapter.TaggedPacketConn); ok {
		return taggedConn, nil
	}
	return adapter.NewTaggedPacketConn(conn, realTag(outbound)), nil
}

func (s *MutableSelector) NewConnectionEx(ctx context.Context, conn net.Conn, metadata A.InboundContext, onClose network.CloseHandlerFunc) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.NewConnectionEx", trace.WithAttributes(
		attribute.String("inbound", s.Now()),
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
	selected := s.selected.Load()
	if selected == nil {
		err := errors.New("no outbound available")
		s.logger.ErrorContext(ctx, err)
		wrappedOnClose(err)
		return
	}
	if handler, isHandler := selected.(A.ConnectionHandlerEx); isHandler {
		handler.NewConnectionEx(ctx, conn, metadata, wrappedOnClose)
		return
	}
	s.connMgr.NewConnection(ctx, selected, conn, metadata, wrappedOnClose)
}

func (s *MutableSelector) NewPacketConnectionEx(ctx context.Context, conn network.PacketConn, metadata A.InboundContext, onClose network.CloseHandlerFunc) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "MutableSelector.NewPacketConnectionEx", trace.WithAttributes(
		attribute.String("inbound", s.Now()),
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
	selected := s.selected.Load()
	if selected == nil {
		err := errors.New("no outbound available")
		s.logger.ErrorContext(ctx, err)
		wrappedOnClose(err)
		return
	}
	if handler, isHandler := selected.(A.PacketConnectionHandlerEx); isHandler {
		handler.NewPacketConnectionEx(ctx, conn, metadata, wrappedOnClose)
		return
	}
	s.connMgr.NewPacketConnection(ctx, selected, conn, metadata, wrappedOnClose)
}
