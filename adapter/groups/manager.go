package groups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"

	"github.com/getlantern/lantern-box/adapter"
)

const (
	pollInterval = 5 * time.Second
	forceAfter   = 15 * time.Minute
)

var (
	ErrIsClosed = errors.New("manager is closed")
)

type ConnectionManager interface {
	Connections() []trafficontrol.TrackerMetadata
}

type MutableGroupManager struct {
	outboundMgr A.OutboundManager
	endpointMgr A.EndpointManager
	groups      map[string]adapter.MutableOutboundGroup

	removalQueue *removalQueue
	closed       atomic.Bool
	mu           sync.Mutex
}

func NewMutableGroupManager(
	logger logger.ContextLogger,
	outboundManager A.OutboundManager,
	endpointManager A.EndpointManager,
	connectionManager ConnectionManager,
	groups []adapter.MutableOutboundGroup,
) *MutableGroupManager {
	gMap := make(map[string]adapter.MutableOutboundGroup)
	for _, group := range groups {
		gMap[group.Tag()] = group
	}
	return &MutableGroupManager{
		outboundMgr: outboundManager,
		endpointMgr: endpointManager,
		groups:      gMap,
		removalQueue: newRemovalQueue(
			logger, outboundManager, endpointManager, connectionManager, pollInterval, forceAfter,
		),
	}
}

func (m *MutableGroupManager) Close() {
	if m.closed.Swap(true) {
		return
	}
	m.removalQueue.close()
}

func (m *MutableGroupManager) OutboundGroup(tag string) (adapter.MutableOutboundGroup, bool) {
	group, found := m.groups[tag]
	return group, found
}

func (m *MutableGroupManager) OutboundGroups() []adapter.MutableOutboundGroup {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Values(m.groups))
}

// CreateOutboundForGroup creates an outbound and adds it to the specified group.
func (m *MutableGroupManager) CreateOutboundForGroup(
	ctx context.Context,
	router A.Router,
	logger log.ContextLogger,
	group, tag, typ string,
	options any,
) error {
	return m.createForGroup(ctx, m.outboundMgr, router, logger, group, tag, typ, options)
}

// CreateEndpointForGroup creates an endpoint and adds it to the specified group.
func (m *MutableGroupManager) CreateEndpointForGroup(
	ctx context.Context,
	router A.Router,
	logger log.ContextLogger,
	group, tag, typ string,
	options any,
) error {
	return m.createForGroup(ctx, m.endpointMgr, router, logger, group, tag, typ, options)
}

type outboundManager interface {
	Create(ctx context.Context, router A.Router, logger log.ContextLogger, tag string, outboundType string, options any) error
}

func (m *MutableGroupManager) createForGroup(
	ctx context.Context,
	mgr outboundManager,
	router A.Router,
	logger log.ContextLogger,
	group, tag, typ string,
	options any,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return errors.New("manager is closed")
	}

	outGroup, found := m.groups[group]
	if !found {
		return fmt.Errorf("group %s not found", group)
	}

	if err := mgr.Create(ctx, router, logger, tag, typ, options); err != nil {
		return err
	}
	return m.addToGroup(outGroup, tag)
}

func (m *MutableGroupManager) AddToGroup(group, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return errors.New("manager is closed")
	}

	outGroup, found := m.groups[group]
	if !found {
		return fmt.Errorf("group %s not found", group)
	}
	return m.addToGroup(outGroup, tag)
}

func (m *MutableGroupManager) addToGroup(outGroup adapter.MutableOutboundGroup, tag string) error {
	n, err := outGroup.Add(tag)
	if err != nil || n == 0 {
		if err == nil {
			err = errors.New("unknown")
		}
		return fmt.Errorf("failed to add %s to %s: %w", tag, outGroup.Tag(), err)
	}
	// remove from removal queue in case it was scheduled for removal
	m.removalQueue.dequeue(tag)
	return nil
}

// SetURLOverrides updates the per-outbound URL test overrides for the specified group.
// The group must implement [adapter.URLOverrideSetter].
func (m *MutableGroupManager) SetURLOverrides(group string, overrides map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	outGroup, ok := m.groups[group]
	if !ok {
		return fmt.Errorf("group %q not found", group)
	}
	setter, ok := outGroup.(adapter.URLOverrideSetter)
	if !ok {
		return fmt.Errorf("group %q does not support URL overrides", group)
	}
	setter.SetURLOverrides(overrides)
	return nil
}

// RemoveFromGroup removes an outbound/endpoint from the specified group.
func (m *MutableGroupManager) RemoveFromGroup(group, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return ErrIsClosed
	}
	groupObj, found := m.groups[group]
	if !found {
		return fmt.Errorf("group %s not found", group)
	}

	n, err := groupObj.Remove(tag)
	if err != nil || n == 0 {
		if err == nil {
			err = errors.New("unknown")
		}
		return fmt.Errorf("failed to remove %s from %s: %w", tag, group, err)
	}

	outbound, exists := m.outboundMgr.Outbound(tag)
	if !exists {
		return nil
	}
	// we don't want to delete outbound groups themselves
	if _, isOutboundGroup := outbound.(A.OutboundGroup); !isOutboundGroup {
		_, isEndpoint := m.endpointMgr.Get(tag)
		m.removalQueue.enqueue(tag, isEndpoint)
	}
	return nil
}

// removalQueue handles delayed removal of outbounds/endpoints.
type removalQueue struct {
	logger       log.ContextLogger
	outMgr       A.OutboundManager
	epMgr        A.EndpointManager
	connMgr      ConnectionManager
	pending      map[string]item
	pollInterval time.Duration
	forceAfter   time.Duration
	mu           sync.RWMutex
	running      atomic.Bool
	done         chan struct{}
	once         sync.Once
}

type item struct {
	tag        string
	isEndpoint bool
	addedAt    time.Time
}

func newRemovalQueue(
	logger log.ContextLogger,
	outMgr A.OutboundManager,
	epMgr A.EndpointManager,
	connMgr ConnectionManager,
	pollInterval, forceAfter time.Duration,
) *removalQueue {
	return &removalQueue{
		logger:       logger,
		outMgr:       outMgr,
		epMgr:        epMgr,
		connMgr:      connMgr,
		pending:      make(map[string]item),
		pollInterval: pollInterval,
		forceAfter:   forceAfter,
		done:         make(chan struct{}),
	}
}

func (rq *removalQueue) enqueue(tag string, isEndpoint bool) {
	select {
	case <-rq.done:
		return
	default:
	}

	rq.mu.Lock()
	defer rq.mu.Unlock()
	if _, exists := rq.pending[tag]; exists {
		return
	}
	rq.pending[tag] = item{
		tag:        tag,
		isEndpoint: isEndpoint,
		addedAt:    time.Now(),
	}
	if !rq.running.Load() {
		go rq.checkLoop()
	}
}

func (rq *removalQueue) dequeue(tag string) {
	rq.mu.Lock()
	delete(rq.pending, tag)
	rq.mu.Unlock()
}

func (rq *removalQueue) checkLoop() {
	if !rq.running.CompareAndSwap(false, true) {
		return
	}
	defer rq.running.Store(false)

	rq.checkPending()
	ticker := time.NewTicker(rq.pollInterval)
	defer ticker.Stop()
	for {
		rq.mu.Lock()
		if len(rq.pending) == 0 {
			rq.mu.Unlock()
			return
		}
		rq.mu.Unlock()
		select {
		case <-ticker.C:
			rq.checkPending()
		case <-rq.done:
			return
		}
	}
}

// checkPending checks the pending removal items and removes them if they are not in use or have
// exceeded the forceAfter duration.
func (rq *removalQueue) checkPending() {
	rq.mu.RLock()
	pending := make(map[string]item, len(rq.pending))
	maps.Copy(pending, rq.pending)
	rq.mu.RUnlock()

	hasConns := make(map[string]bool, len(rq.pending))
	for _, conn := range rq.connMgr.Connections() {
		if _, exists := pending[conn.Outbound]; exists {
			hasConns[conn.Outbound] = hasConns[conn.Outbound] || conn.ClosedAt.IsZero()
		}
	}

	var toRemove []string
	for tag, item := range pending {
		if !hasConns[tag] || time.Since(item.addedAt) > rq.forceAfter {
			toRemove = append(toRemove, tag)
		}
	}

	if len(toRemove) > 0 {
		defer func() {
			if err := recover(); err != nil {
				rq.logger.Error("panic during outbound/endpoint removal", "error", err)
			}
		}()
		rq.mu.Lock()
		defer rq.mu.Unlock()
		for _, tag := range toRemove {
			item, exists := rq.pending[tag]
			if !exists {
				continue
			}

			if item.isEndpoint {
				rq.logger.Debug("removing endpoint", "tag", tag)
				if _, exists = rq.epMgr.Get(tag); !exists {
					rq.logger.Trace("endpoint already removed", "tag", tag)
					delete(rq.pending, tag)
					continue
				}
				if err := rq.epMgr.Remove(tag); err != nil {
					rq.logger.Warn("failed to remove endpoint", slog.Any("error", err), slog.String("tag", tag))
				}
			} else {
				rq.logger.Debug("removing outbound", "tag", tag)
				// double check if outbound/endpoint still exists
				if _, exists = rq.outMgr.Outbound(tag); !exists {
					rq.logger.Trace("outbound already removed", "tag", tag)
					delete(rq.pending, tag)
					continue
				}
				if err := rq.outMgr.Remove(tag); err != nil {
					rq.logger.Warn("failed to remove outbound", slog.Any("error", err), slog.String("tag", tag))
				}
			}
			delete(rq.pending, tag)
		}
	}
}

func (rq *removalQueue) close() {
	rq.once.Do(func() {
		close(rq.done)
	})
}
