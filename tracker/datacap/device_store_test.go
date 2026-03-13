package datacap

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDataCapAPI implements DataCapAPI for testing.
type mockDataCapAPI struct {
	mu              sync.Mutex
	reportCalls     int
	syncCalls       int
	reportBatches   []*UsageBatch
	syncResponses   map[string]*DeviceState
	reportErr       error
	syncErr         error
	reportResults   *ReportUsageResult
	totalBytesUploaded atomic.Int64
}

func newMockAPI() *mockDataCapAPI {
	return &mockDataCapAPI{
		syncResponses: make(map[string]*DeviceState),
	}
}

func (m *mockDataCapAPI) ReportUsage(ctx context.Context, batch *UsageBatch) (*ReportUsageResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reportCalls++
	m.reportBatches = append(m.reportBatches, batch)
	for _, r := range batch.Records {
		m.totalBytesUploaded.Add(r.BytesUsed)
	}
	if m.reportErr != nil {
		return nil, m.reportErr
	}
	if m.reportResults != nil {
		return m.reportResults, nil
	}
	// Default: all succeed
	result := &ReportUsageResult{}
	for _, r := range batch.Records {
		result.Results = append(result.Results, UsageResultEntry{
			DeviceID: r.DeviceID,
			Success:  true,
		})
	}
	return result, nil
}

func (m *mockDataCapAPI) SyncDeviceState(ctx context.Context, deviceID string) (*DeviceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncCalls++
	if m.syncErr != nil {
		return nil, m.syncErr
	}
	if state, ok := m.syncResponses[deviceID]; ok {
		return state, nil
	}
	return &DeviceState{
		DeviceID:   deviceID,
		BytesUsed:  0,
		CapLimit:   10 * 1024 * 1024 * 1024, // 10 GB
		ExpiryTime: time.Now().Add(24 * time.Hour).Unix(),
	}, nil
}

func TestDeviceUsageStore_Report_AccumulatesBytes(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: time.Hour, // Don't upload during test
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx := context.Background()
	status, err := store.Report(ctx, &DataCapReport{
		DeviceID:    "dev1",
		CountryCode: "US",
		Platform:    "android",
		BytesUsed:   1000,
	})
	require.NoError(t, err)
	assert.False(t, status.Throttle)

	status, err = store.Report(ctx, &DataCapReport{
		DeviceID:  "dev1",
		BytesUsed: 2000,
	})
	require.NoError(t, err)
	assert.False(t, status.Throttle)

	store.mu.RLock()
	device := store.devices["dev1"]
	assert.Equal(t, int64(3000), device.PendingBytes)
	assert.Equal(t, int64(3000), device.BytesUsed)
	store.mu.RUnlock()
}

func TestDeviceUsageStore_Report_ThrottlesWhenOverCap(t *testing.T) {
	api := newMockAPI()
	api.syncResponses["dev1"] = &DeviceState{
		DeviceID:   "dev1",
		BytesUsed:  999,
		CapLimit:   1000,
		ExpiryTime: time.Now().Add(24 * time.Hour).Unix(),
	}
	store := NewDeviceUsageStore(api, StoreOptions{
		CacheTTL: time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx := context.Background()
	// First report triggers sync (device not seen yet) — BytesUsed becomes 999+100=1099
	status, err := store.Report(ctx, &DataCapReport{
		DeviceID:  "dev1",
		BytesUsed: 100,
	})
	require.NoError(t, err)
	assert.True(t, status.Throttle, "should throttle when over cap")
	assert.Equal(t, int64(1000), status.CapLimit)
}

func TestDeviceUsageStore_BatchUpload(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: 50 * time.Millisecond,
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.Start(ctx)
	defer store.Stop(ctx)

	// Report some bytes
	store.Report(ctx, &DataCapReport{
		DeviceID:  "dev1",
		BytesUsed: 5000,
	})
	store.Report(ctx, &DataCapReport{
		DeviceID:  "dev2",
		BytesUsed: 3000,
	})

	// Wait for batch upload
	time.Sleep(200 * time.Millisecond)

	api.mu.Lock()
	assert.GreaterOrEqual(t, api.reportCalls, 1, "should have uploaded at least one batch")
	api.mu.Unlock()

	// Pending bytes should be cleared
	store.mu.RLock()
	if dev1, ok := store.devices["dev1"]; ok {
		assert.Equal(t, int64(0), dev1.PendingBytes, "pending bytes should be 0 after upload")
		assert.Equal(t, int64(0), dev1.UploadingBytes, "uploading bytes should be 0 after upload")
	}
	store.mu.RUnlock()
}

func TestDeviceUsageStore_BatchUpload_FailureRestoresBytes(t *testing.T) {
	api := newMockAPI()
	api.reportErr = fmt.Errorf("network error")
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: time.Hour,
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx := context.Background()
	store.Report(ctx, &DataCapReport{
		DeviceID:  "dev1",
		BytesUsed: 5000,
	})

	// Manually trigger upload
	err := store.uploadBatch(ctx)
	assert.Error(t, err)

	// Bytes should be restored to pending
	store.mu.RLock()
	dev1 := store.devices["dev1"]
	assert.Equal(t, int64(5000), dev1.PendingBytes, "bytes should be restored on failure")
	assert.Equal(t, int64(0), dev1.UploadingBytes)
	store.mu.RUnlock()
}

func TestDeviceUsageStore_SyncDeviceState(t *testing.T) {
	api := newMockAPI()
	api.syncResponses["dev1"] = &DeviceState{
		DeviceID:   "dev1",
		BytesUsed:  50000,
		CapLimit:   100000,
		ExpiryTime: 1700000000,
	}
	store := NewDeviceUsageStore(api, StoreOptions{
		CacheTTL: 0, // Always sync
	}, log.NewNOPFactory().Logger())

	ctx := context.Background()
	status, err := store.Report(ctx, &DataCapReport{
		DeviceID:    "dev1",
		CountryCode: "US",
		Platform:    "android",
		BytesUsed:   1000,
	})
	require.NoError(t, err)
	assert.False(t, status.Throttle)
	assert.Equal(t, int64(100000), status.CapLimit)

	store.mu.RLock()
	dev := store.devices["dev1"]
	// BytesUsed = synced(50000) + pending(1000) + uploading(0) + delta(1000) = 52000
	// Actually: sync sets BytesUsed = resp.BytesUsed + pending + uploading
	// Then Report adds delta again. Let me check the flow:
	// 1. Report sees dev1 doesn't exist → needSync=true
	// 2. syncDeviceState creates device with BytesUsed=50000+0+0=50000
	// 3. Report re-reads device, exists=true, adds delta: PendingBytes+=1000, BytesUsed+=1000
	// So: BytesUsed=51000, PendingBytes=1000
	assert.Equal(t, int64(51000), dev.BytesUsed)
	assert.Equal(t, int64(1000), dev.PendingBytes)
	store.mu.RUnlock()
}

func TestDeviceUsageStore_CleanupStaleDevices(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{
		CacheTTL: time.Hour, // staleThreshold = max(cacheTTL, 1h) = 1h
	}, log.NewNOPFactory().Logger())

	// Add a device with old activity time
	store.mu.Lock()
	store.devices["old-dev"] = &DeviceUsage{
		DeviceID:         "old-dev",
		PendingBytes:     0,
		UploadingBytes:   0,
		LastActivityTime: time.Now().Add(-2 * time.Hour),
	}
	store.devices["active-dev"] = &DeviceUsage{
		DeviceID:         "active-dev",
		PendingBytes:     100,
		LastActivityTime: time.Now().Add(-2 * time.Hour),
	}
	store.mu.Unlock()

	store.cleanupStaleDevices()

	store.mu.RLock()
	_, oldExists := store.devices["old-dev"]
	_, activeExists := store.devices["active-dev"]
	store.mu.RUnlock()

	assert.False(t, oldExists, "stale device should be removed")
	assert.True(t, activeExists, "device with pending bytes should not be removed")
}

func TestDeviceUsageStore_ConcurrentReports(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: 50 * time.Millisecond,
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.Start(ctx)
	defer store.Stop(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				store.Report(ctx, &DataCapReport{
					DeviceID:  fmt.Sprintf("dev-%d", id%3),
					BytesUsed: 100,
				})
			}
		}(i)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	// Verify no panics and all bytes tracked
	// Total: 10 goroutines * 100 reports * 100 bytes = 100,000 bytes across 3 devices
	store.mu.RLock()
	totalBytes := int64(0)
	totalPending := int64(0)
	for _, dev := range store.devices {
		totalBytes += dev.BytesUsed
		totalPending += dev.PendingBytes
	}
	store.mu.RUnlock()

	assert.Equal(t, int64(100000), totalBytes, "all bytes should be tracked")
}

func TestDeviceUsageStore_GracefulShutdown(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: time.Hour,
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx, cancel := context.WithCancel(context.Background())
	store.Start(ctx)

	// Report some bytes
	store.Report(ctx, &DataCapReport{
		DeviceID:  "dev1",
		BytesUsed: 5000,
	})

	// Stop should upload final batch
	cancel()
	store.Stop(ctx)

	assert.Equal(t, int64(5000), api.totalBytesUploaded.Load(), "final batch should upload pending bytes")
}

func TestDeviceUsageStore_ImplementsReportSink(t *testing.T) {
	api := newMockAPI()
	store := NewDeviceUsageStore(api, StoreOptions{}, log.NewNOPFactory().Logger())

	// Verify DeviceUsageStore implements ReportSink
	var sink ReportSink = store
	_ = sink
}

func TestDeviceUsageStore_PartialBatchSuccess(t *testing.T) {
	api := newMockAPI()
	api.reportResults = &ReportUsageResult{
		Results: []UsageResultEntry{
			{DeviceID: "dev1", Success: true},
			{DeviceID: "dev2", Success: false, Error: "quota exceeded"},
		},
	}
	store := NewDeviceUsageStore(api, StoreOptions{
		BatchInterval: time.Hour,
		CacheTTL:      time.Hour,
	}, log.NewNOPFactory().Logger())

	ctx := context.Background()
	store.Report(ctx, &DataCapReport{DeviceID: "dev1", BytesUsed: 1000})
	store.Report(ctx, &DataCapReport{DeviceID: "dev2", BytesUsed: 2000})

	err := store.uploadBatch(ctx)
	require.NoError(t, err)

	store.mu.RLock()
	dev1 := store.devices["dev1"]
	dev2 := store.devices["dev2"]
	assert.Equal(t, int64(0), dev1.PendingBytes, "dev1 succeeded, pending should be 0")
	assert.Equal(t, int64(0), dev1.UploadingBytes)
	assert.Equal(t, int64(2000), dev2.PendingBytes, "dev2 failed, bytes should be restored")
	assert.Equal(t, int64(0), dev2.UploadingBytes)
	store.mu.RUnlock()
}
