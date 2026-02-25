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
