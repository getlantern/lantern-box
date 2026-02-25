package groups

import (
	"slices"
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

// mockURLOverrideGroup implements both MutableOutboundGroup and URLOverrideSetter.
type mockURLOverrideGroup struct {
	lbAdapter.MutableOutboundGroup
	urlOverrides map[string]string
}

func (m *mockURLOverrideGroup) SetURLOverrides(overrides map[string]string) {
	m.urlOverrides = overrides
}

// mockPlainGroup implements MutableOutboundGroup but NOT URLOverrideSetter.
type mockPlainGroup struct {
	lbAdapter.MutableOutboundGroup
}

type mockOutboundManager struct {
	adapter.OutboundManager
	tags []string
	mu   sync.Mutex
}

func (m *mockOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	return nil, slices.Contains(m.tags, tag)
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
