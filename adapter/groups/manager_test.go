package groups

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
)

func TestRemovalQueue(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	tag := "outbound"
	tests := []struct {
		name       string
		outMgr     *mockOutboundManager
		epMgr      *mockEndpointManager
		connMgr    *mockConnectionManager
		pending    map[string]item
		forceAfter time.Duration
		assertFn   func(t *testing.T, rq *removalQueue)
	}{
		{
			name:       "remove outbound",
			outMgr:     &mockOutboundManager{tags: []string{tag}},
			connMgr:    &mockConnectionManager{},
			pending:    map[string]item{tag: {tag, false, time.Now()}},
			forceAfter: time.Minute,
			assertFn: func(t *testing.T, rq *removalQueue) {
				assert.NotContains(t, rq.outMgr.(*mockOutboundManager).tags, tag, "tag should be removed")
			},
		},
		{
			name:       "remove endpoint",
			epMgr:      &mockEndpointManager{tags: []string{tag}},
			connMgr:    &mockConnectionManager{},
			pending:    map[string]item{tag: {tag, true, time.Now()}},
			forceAfter: time.Minute,
			assertFn: func(t *testing.T, rq *removalQueue) {
				assert.NotContains(t, rq.epMgr.(*mockEndpointManager).tags, tag, "tag should be removed")
			},
		},
		{
			name:   "force removal after duration",
			outMgr: &mockOutboundManager{tags: []string{tag}},
			connMgr: &mockConnectionManager{
				conns: []trafficontrol.TrackerMetadata{{Outbound: tag, ClosedAt: time.Time{}}},
			},
			pending: map[string]item{
				tag: {tag, false, time.Now().Add(-time.Second * 10)},
			},
			forceAfter: time.Second,
			assertFn: func(t *testing.T, rq *removalQueue) {
				assert.NotContains(t, rq.outMgr.(*mockOutboundManager).tags, tag, "tag should be removed")
			},
		},
		{
			name:   "don't remove if still in use",
			outMgr: &mockOutboundManager{tags: []string{tag}},
			connMgr: &mockConnectionManager{
				conns: []trafficontrol.TrackerMetadata{{Outbound: tag, ClosedAt: time.Time{}}},
			},
			pending:    map[string]item{tag: {tag, false, time.Now()}},
			forceAfter: time.Minute,
			assertFn: func(t *testing.T, rq *removalQueue) {
				assert.Contains(t, rq.outMgr.(*mockOutboundManager).tags, tag, "tag should still be present")
			},
		},
		{
			name:   "don't remove if re-added",
			outMgr: &mockOutboundManager{tags: []string{tag}},
			connMgr: &mockConnectionManager{
				conns: []trafficontrol.TrackerMetadata{{Outbound: tag, ClosedAt: time.Time{}}},
			},
			pending:    map[string]item{tag: {tag, false, time.Now()}},
			forceAfter: time.Minute,
			assertFn: func(t *testing.T, rq *removalQueue) {
				require.Contains(t, rq.outMgr.(*mockOutboundManager).tags, tag, "tag should still be present before re-adding")

				rq.dequeue(tag)
				rq.connMgr.Connections()[0].ClosedAt = time.Now() // simulate connection closed
				rq.checkPending()
				assert.Contains(t, rq.outMgr.(*mockOutboundManager).tags, tag, "tag should still be present after re-adding")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rq := &removalQueue{
				logger:     logger,
				outMgr:     tt.outMgr,
				epMgr:      tt.epMgr,
				connMgr:    tt.connMgr,
				pending:    tt.pending,
				forceAfter: tt.forceAfter,
			}
			rq.checkPending()
			tt.assertFn(t, rq)
		})
	}
}

func TestSetURLOverrides(t *testing.T) {
	logger := log.NewNOPFactory().Logger()

	t.Run("delegates to URLOverrideSetter group", func(t *testing.T) {
		mock := &mockURLOverrideGroup{urlOverrides: nil}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"auto-test": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		overrides := map[string]string{"out1": "https://example.com/cb"}
		err := mgr.SetURLOverrides("auto-test", overrides)
		require.NoError(t, err)
		assert.Equal(t, overrides, mock.urlOverrides)
	})

	t.Run("error when group not found", func(t *testing.T) {
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		err := mgr.SetURLOverrides("nonexistent", nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("error when group does not implement URLOverrideSetter", func(t *testing.T) {
		mock := &mockPlainGroup{}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"plain": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		err := mgr.SetURLOverrides("plain", nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "does not support URL overrides")
	})

	t.Run("error when manager is closed", func(t *testing.T) {
		mock := &mockURLOverrideGroup{}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"auto-test": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		mgr.closed.Store(true)
		err := mgr.SetURLOverrides("auto-test", nil)
		assert.ErrorIs(t, err, ErrIsClosed)
	})
}

func TestCheckOutbounds(t *testing.T) {
	logger := log.NewNOPFactory().Logger()

	t.Run("delegates to OutboundChecker group", func(t *testing.T) {
		mock := &mockCheckerGroup{}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"auto-test": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		err := mgr.CheckOutbounds("auto-test")
		require.NoError(t, err)
		assert.True(t, mock.checked)
	})

	t.Run("error when group not found", func(t *testing.T) {
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		err := mgr.CheckOutbounds("nonexistent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("error when group does not implement OutboundChecker", func(t *testing.T) {
		mock := &mockPlainGroup{}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"plain": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		err := mgr.CheckOutbounds("plain")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "does not support outbound checking")
	})

	t.Run("error when manager is closed", func(t *testing.T) {
		mock := &mockCheckerGroup{}
		mgr := &MutableGroupManager{
			groups: map[string]lbAdapter.MutableOutboundGroup{"auto-test": mock},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
		mgr.closed.Store(true)
		err := mgr.CheckOutbounds("auto-test")
		assert.ErrorIs(t, err, ErrIsClosed)
		assert.False(t, mock.checked)
	})
}

func TestRemoveFromGroup(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	tag := "outbound"

	newMgr := func(group lbAdapter.MutableOutboundGroup) *MutableGroupManager {
		return &MutableGroupManager{
			outboundMgr: &mockOutboundManager{tags: []string{tag}},
			endpointMgr: &mockEndpointManager{},
			groups:      map[string]lbAdapter.MutableOutboundGroup{"auto-test": group},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
	}

	pendingContains := func(mgr *MutableGroupManager, tag string) bool {
		mgr.removalQueue.mu.RLock()
		defer mgr.removalQueue.mu.RUnlock()
		_, found := mgr.removalQueue.pending[tag]
		return found
	}

	t.Run("removes and enqueues outbound", func(t *testing.T) {
		mgr := newMgr(&mockRemovableGroup{removeN: 1})
		require.NoError(t, mgr.RemoveFromGroup("auto-test", tag))
		assert.True(t, pendingContains(mgr, tag), "outbound should be queued for removal")
	})

	t.Run("closed group treated as removed", func(t *testing.T) {
		mgr := newMgr(&mockRemovableGroup{removeErr: lbAdapter.ErrGroupClosed})
		require.NoError(t, mgr.RemoveFromGroup("auto-test", tag),
			"removal racing group teardown must not error")
		assert.True(t, pendingContains(mgr, tag), "outbound should still be queued for cleanup")
	})

	t.Run("tag not in group is idempotent", func(t *testing.T) {
		mgr := newMgr(&mockRemovableGroup{removeN: 0})
		assert.NoError(t, mgr.RemoveFromGroup("auto-test", tag))
		assert.True(t, pendingContains(mgr, tag), "outbound should still be queued for cleanup")
	})

	t.Run("other errors propagate", func(t *testing.T) {
		mgr := newMgr(&mockRemovableGroup{removeErr: errors.New("boom")})
		err := mgr.RemoveFromGroup("auto-test", tag)
		assert.ErrorContains(t, err, "boom")
	})
}

func TestReplaceInGroup(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	tag := "outbound"

	newMgr := func(group lbAdapter.MutableOutboundGroup) *MutableGroupManager {
		return &MutableGroupManager{
			outboundMgr: &mockOutboundManager{tags: []string{tag}},
			endpointMgr: &mockEndpointManager{},
			groups:      map[string]lbAdapter.MutableOutboundGroup{"auto-test": group},
			removalQueue: newRemovalQueue(
				logger, &mockOutboundManager{}, &mockEndpointManager{}, &mockConnectionManager{},
				pollInterval, forceAfter,
			),
		}
	}

	t.Run("delegates to MemberReplacer group and cancels pending removal", func(t *testing.T) {
		group := &mockReplacerGroup{replaceN: 1}
		mgr := newMgr(group)
		mgr.removalQueue.mu.Lock()
		mgr.removalQueue.pending[tag] = item{tag: tag, addedAt: time.Now()}
		mgr.removalQueue.mu.Unlock()

		require.NoError(t, mgr.ReplaceInGroup("auto-test", tag))
		assert.Equal(t, []string{tag}, group.replaced)
		mgr.removalQueue.mu.RLock()
		_, pending := mgr.removalQueue.pending[tag]
		mgr.removalQueue.mu.RUnlock()
		assert.False(t, pending, "the replacement must not be removed by an earlier scheduled removal")
	})

	t.Run("pending removal cannot run between install and cancel", func(t *testing.T) {
		outMgr := &mockOutboundManager{tags: []string{tag}}
		mgr := newMgr(nil)
		mgr.outboundMgr = outMgr
		mgr.removalQueue = newRemovalQueue(
			logger, outMgr, &mockEndpointManager{}, &mockConnectionManager{}, pollInterval, forceAfter,
		)
		mgr.removalQueue.pending[tag] = item{tag: tag, addedAt: time.Now()}
		cleanupDone := make(chan struct{})
		// Cleanup runs right after the group installs the new outbound.
		mgr.groups["auto-test"] = &mockReplacerGroup{replaceN: 1, onReplace: func() {
			go func() {
				defer close(cleanupDone)
				mgr.removalQueue.checkPending()
			}()
			select {
			case <-cleanupDone:
			case <-time.After(100 * time.Millisecond):
			}
		}}

		require.NoError(t, mgr.ReplaceInGroup("auto-test", tag))
		<-cleanupDone
		outMgr.mu.Lock()
		defer outMgr.mu.Unlock()
		assert.Contains(t, outMgr.tags, tag, "cleanup removed the replacement outbound")
	})

	t.Run("error when tag is not a member", func(t *testing.T) {
		mgr := newMgr(&mockReplacerGroup{replaceN: 0})
		assert.ErrorContains(t, mgr.ReplaceInGroup("auto-test", tag), "not a member")
	})

	t.Run("group errors propagate", func(t *testing.T) {
		mgr := newMgr(&mockReplacerGroup{replaceErr: lbAdapter.ErrGroupClosed})
		assert.ErrorIs(t, mgr.ReplaceInGroup("auto-test", tag), lbAdapter.ErrGroupClosed)
	})

	t.Run("error when group not found", func(t *testing.T) {
		mgr := newMgr(&mockReplacerGroup{})
		assert.ErrorContains(t, mgr.ReplaceInGroup("nonexistent", tag), "not found")
	})

	t.Run("error when group does not implement MemberReplacer", func(t *testing.T) {
		mgr := newMgr(&mockPlainGroup{})
		assert.ErrorContains(t, mgr.ReplaceInGroup("auto-test", tag), "does not support replacing members")
	})

	t.Run("error when manager is closed", func(t *testing.T) {
		group := &mockReplacerGroup{replaceN: 1}
		mgr := newMgr(group)
		mgr.closed.Store(true)
		assert.ErrorIs(t, mgr.ReplaceInGroup("auto-test", tag), ErrIsClosed)
		assert.Empty(t, group.replaced)
	})
}

// mockReplacerGroup implements both MutableOutboundGroup and MemberReplacer.
type mockReplacerGroup struct {
	lbAdapter.MutableOutboundGroup
	replaceN   int
	replaceErr error
	replaced   []string
	onReplace  func()
}

func (m *mockReplacerGroup) Replace(tags ...string) (int, error) {
	m.replaced = append(m.replaced, tags...)
	if m.onReplace != nil {
		m.onReplace()
	}
	return m.replaceN, m.replaceErr
}

// mockRemovableGroup implements MutableOutboundGroup with a configurable Remove.
type mockRemovableGroup struct {
	lbAdapter.MutableOutboundGroup
	removeN   int
	removeErr error
}

func (m *mockRemovableGroup) Remove(tags ...string) (int, error) {
	return m.removeN, m.removeErr
}

// mockCheckerGroup implements both MutableOutboundGroup and OutboundChecker.
type mockCheckerGroup struct {
	lbAdapter.MutableOutboundGroup
	checked bool
}

func (m *mockCheckerGroup) CheckOutbounds() {
	m.checked = true
}

// mockURLOverrideGroup implements both MutableOutboundGroup and URLOverrideSetter.
type mockURLOverrideGroup struct {
	lbAdapter.MutableOutboundGroup
	urlOverrides map[string]string
}

func (m *mockURLOverrideGroup) SetURLOverrides(overrides map[string]string) {
	m.urlOverrides = overrides
}

// mockPlainGroup implements MutableOutboundGroup but NOT URLOverrideSetter or OutboundChecker.
type mockPlainGroup struct {
	lbAdapter.MutableOutboundGroup
}

type mockOutboundManager struct {
	adapter.OutboundManager
	tags     []string
	mu       sync.Mutex
	onCreate func()
}

func (m *mockOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, slices.Contains(m.tags, tag)
}

func (m *mockOutboundManager) Create(_ context.Context, _ adapter.Router, _ log.ContextLogger, tag, _ string, _ any) error {
	m.mu.Lock()
	if !slices.Contains(m.tags, tag) {
		m.tags = append(m.tags, tag)
	}
	m.mu.Unlock()
	if m.onCreate != nil {
		m.onCreate()
	}
	return nil
}

func (m *mockOutboundManager) Remove(tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx := slices.Index(m.tags, tag); idx != -1 {
		m.tags = append(m.tags[:idx], m.tags[idx+1:]...)
	}
	return nil
}

type mockEndpointManager struct {
	adapter.EndpointManager
	tags []string
	mu   sync.Mutex
}

func (m *mockEndpointManager) Get(tag string) (adapter.Endpoint, bool) {
	return nil, slices.Contains(m.tags, tag)
}

func (m *mockEndpointManager) Remove(tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx := slices.Index(m.tags, tag); idx != -1 {
		m.tags = append(m.tags[:idx], m.tags[idx+1:]...)
	}
	return nil
}

type mockConnectionManager struct {
	conns []trafficontrol.TrackerMetadata
}

func (m *mockConnectionManager) Connections() []trafficontrol.TrackerMetadata {
	if m == nil {
		return nil
	}
	return m.conns
}

func TestRemovalQueue_CheckPendingConcurrentWithEnqueue(t *testing.T) {
	rq := newRemovalQueue(
		log.NewNOPFactory().Logger(), &mockOutboundManager{}, &mockEndpointManager{},
		&mockConnectionManager{}, time.Hour, time.Hour,
	)
	defer rq.close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 2000 {
			rq.checkPending()
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 2000 {
			tag := "tag" + strconv.Itoa(i%10)
			rq.enqueue(tag, false)
			rq.dequeue(tag)
		}
	}()
	wg.Wait()
}

func TestCreateOutboundForGroup_PendingRemovalCannotRemoveNewOutbound(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	tag := "outbound"
	outMgr := &mockOutboundManager{}
	mgr := &MutableGroupManager{
		outboundMgr: outMgr,
		endpointMgr: &mockEndpointManager{},
		groups:      map[string]lbAdapter.MutableOutboundGroup{"auto-test": &mockAddGroup{}},
		removalQueue: newRemovalQueue(
			logger, outMgr, &mockEndpointManager{}, &mockConnectionManager{}, pollInterval, forceAfter,
		),
	}
	// The withdrawn outbound's removal is still queued when it is re-added.
	mgr.removalQueue.pending[tag] = item{tag: tag, addedAt: time.Now()}
	cleanupDone := make(chan struct{})
	outMgr.onCreate = func() {
		go func() {
			defer close(cleanupDone)
			mgr.removalQueue.checkPending()
		}()
		select {
		case <-cleanupDone:
		case <-time.After(100 * time.Millisecond):
		}
	}

	require.NoError(t, mgr.CreateOutboundForGroup(context.Background(), nil, logger, "auto-test", tag, "type", nil))
	<-cleanupDone
	outMgr.mu.Lock()
	defer outMgr.mu.Unlock()
	assert.Contains(t, outMgr.tags, tag, "cleanup removed the newly created outbound")
}

func TestAddToGroup_PendingRemovalCannotRemoveAddedOutbound(t *testing.T) {
	logger := log.NewNOPFactory().Logger()
	tag := "outbound"
	outMgr := &mockOutboundManager{tags: []string{tag}}
	cleanupDone := make(chan struct{})
	var rq *removalQueue
	group := &mockAddGroup{onAdd: func() {
		go func() {
			defer close(cleanupDone)
			rq.checkPending()
		}()
		select {
		case <-cleanupDone:
		case <-time.After(100 * time.Millisecond):
		}
	}}
	rq = newRemovalQueue(logger, outMgr, &mockEndpointManager{}, &mockConnectionManager{}, pollInterval, forceAfter)
	mgr := &MutableGroupManager{
		outboundMgr:  outMgr,
		endpointMgr:  &mockEndpointManager{},
		groups:       map[string]lbAdapter.MutableOutboundGroup{"auto-test": group},
		removalQueue: rq,
	}
	rq.pending[tag] = item{tag: tag, addedAt: time.Now()}

	require.NoError(t, mgr.AddToGroup("auto-test", tag))
	<-cleanupDone
	outMgr.mu.Lock()
	defer outMgr.mu.Unlock()
	assert.Contains(t, outMgr.tags, tag, "cleanup removed the outbound just added to the group")
}

// mockAddGroup implements MutableOutboundGroup with an Add that accepts every tag.
type mockAddGroup struct {
	lbAdapter.MutableOutboundGroup
	onAdd func()
}

func (m *mockAddGroup) Add(tags ...string) (int, error) {
	if m.onAdd != nil {
		m.onAdd()
	}
	return len(tags), nil
}

func (m *mockAddGroup) Tag() string { return "auto-test" }
