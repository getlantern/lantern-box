package datacap

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sagernet/sing-box/log"
	"golang.org/x/sync/singleflight"
)

// DeviceUsage represents data cap information for a specific device.
type DeviceUsage struct {
	DeviceID    string
	CountryCode string
	Platform    string

	PendingBytes   int64
	UploadingBytes int64

	BytesUsed  int64
	CapLimit   int64
	ExpiryTime int64

	LastSyncTime     time.Time
	LastActivityTime time.Time
}

// StoreOptions configures the DeviceUsageStore.
type StoreOptions struct {
	BatchInterval time.Duration
	CacheTTL      time.Duration
}

func (o StoreOptions) withDefaults() StoreOptions {
	if o.BatchInterval <= 0 {
		o.BatchInterval = 30 * time.Second
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 5 * time.Minute
	}
	return o
}

// DeviceUsageStore replaces the sidecar container by running datacap aggregation,
// batch upload, and state sync in-process.
type DeviceUsageStore struct {
	api     DataCapAPI
	opts    StoreOptions
	logger  log.ContextLogger
	devices map[string]*DeviceUsage
	mu      sync.RWMutex

	syncGroup singleflight.Group

	shutdown chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewDeviceUsageStore creates a new in-process datacap store.
func NewDeviceUsageStore(api DataCapAPI, opts StoreOptions, logger log.ContextLogger) *DeviceUsageStore {
	opts = opts.withDefaults()
	return &DeviceUsageStore{
		api:      api,
		opts:     opts,
		logger:   logger,
		devices:  make(map[string]*DeviceUsage),
		shutdown: make(chan struct{}),
	}
}

// Start launches background goroutines for batch uploading and cleanup.
func (s *DeviceUsageStore) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.backgroundLoop(ctx)
	}()
}

// Stop signals the background loop to exit, waits for it to finish, then
// uploads a final batch. The ctx bounds the overall shutdown time.
// It is idempotent and safe to call multiple times.
func (s *DeviceUsageStore) Stop(ctx context.Context) {
	s.stopOnce.Do(func() {
		close(s.shutdown)
	})

	// Wait for the background loop to exit first so we don't race with
	// its uploadBatch calls.
	doneCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(doneCh)
	}()

	// Use a deadline derived from ctx if available, otherwise use a fixed timeout.
	waitCtx := ctx
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
	}

	select {
	case <-doneCh:
	case <-waitCtx.Done():
		s.logger.Error("timed out waiting for datacap goroutines to finish")
		return
	}

	// Background loop is done — safe to do a final upload.
	uploadCtx, cancel := context.WithTimeout(context.Background(), finalUploadTimeout)
	defer cancel()
	if err := s.uploadBatch(uploadCtx); err != nil {
		s.logger.Error("failed to upload final datacap batch: ", err)
	}
}

// Report implements ReportSink. It accumulates bytes for a device and returns
// the current throttle decision.
func (s *DeviceUsageStore) Report(ctx context.Context, report *DataCapReport) (*DataCapStatus, error) {
	now := time.Now()

	s.mu.Lock()
	device, exists := s.devices[report.DeviceID]
	needSync := !exists || now.Sub(device.LastSyncTime) > s.opts.CacheTTL
	s.mu.Unlock()

	if needSync {
		// Use singleflight to deduplicate concurrent syncs for the same device.
		_, err, _ := s.syncGroup.Do(report.DeviceID, func() (interface{}, error) {
			return nil, s.syncDeviceState(ctx, report.DeviceID)
		})
		if err != nil {
			s.logger.Debug("failed to sync device state for ", report.DeviceID, ": ", err)
		}
	}

	s.mu.Lock()
	device, exists = s.devices[report.DeviceID]
	delta := report.BytesUsed

	if !exists {
		device = &DeviceUsage{
			DeviceID:         report.DeviceID,
			CountryCode:      report.CountryCode,
			Platform:         report.Platform,
			PendingBytes:     delta,
			BytesUsed:        delta,
			ExpiryTime:       now.Add(defaultExpiryDuration).Unix(),
			LastActivityTime: now,
		}
		s.devices[report.DeviceID] = device
	} else {
		device.PendingBytes += delta
		device.BytesUsed += delta
		device.CountryCode = report.CountryCode
		device.Platform = report.Platform
		device.LastActivityTime = now
	}

	throttle := shouldThrottle(device)
	capLimit := device.CapLimit
	expiryTime := device.ExpiryTime
	s.mu.Unlock()

	return &DataCapStatus{
		Throttle:   throttle,
		CapLimit:   capLimit,
		ExpiryTime: expiryTime,
	}, nil
}

func shouldThrottle(device *DeviceUsage) bool {
	if device.CapLimit <= 0 {
		return false
	}
	return device.BytesUsed >= device.CapLimit
}

func (s *DeviceUsageStore) backgroundLoop(ctx context.Context) {
	uploadTicker := time.NewTicker(s.opts.BatchInterval)
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer uploadTicker.Stop()
	defer cleanupTicker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ctx.Done():
			return
		case <-uploadTicker.C:
			if err := s.uploadBatch(ctx); err != nil {
				s.logger.Debug("datacap batch upload failed: ", err)
			}
		case <-cleanupTicker.C:
			s.cleanupStaleDevices()
		}
	}
}

func (s *DeviceUsageStore) uploadBatch(ctx context.Context) error {
	s.mu.Lock()

	records := make([]UsageRecord, 0, len(s.devices))
	devicesToUpload := make(map[string]int64, len(s.devices))

	for _, device := range s.devices {
		if device.PendingBytes > 0 && device.UploadingBytes == 0 {
			bytesToUpload := device.PendingBytes
			device.UploadingBytes = bytesToUpload
			device.PendingBytes = 0
			devicesToUpload[device.DeviceID] = bytesToUpload

			records = append(records, UsageRecord{
				DeviceID:    device.DeviceID,
				BytesUsed:   bytesToUpload,
				CapLimit:    device.CapLimit,
				Platform:    device.Platform,
				CountryCode: device.CountryCode,
			})
		}
	}

	if len(records) == 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	resp, err := s.api.ReportUsage(ctx, &UsageBatch{Records: records})

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		for deviceID, bytesUploading := range devicesToUpload {
			if device, exists := s.devices[deviceID]; exists {
				device.PendingBytes += bytesUploading
				device.UploadingBytes = 0
			}
		}
		return fmt.Errorf("failed to upload usage batch: %w", err)
	}

	successCount := 0
	failCount := 0
	if resp != nil && len(resp.Results) > 0 {
		for _, result := range resp.Results {
			device, exists := s.devices[result.DeviceID]
			if !exists {
				continue
			}
			if result.Success {
				device.UploadingBytes = 0
				successCount++
			} else {
				bytesToRestore := devicesToUpload[result.DeviceID]
				device.PendingBytes += bytesToRestore
				device.UploadingBytes = 0
				failCount++
			}
		}
	} else {
		for deviceID := range devicesToUpload {
			if device, exists := s.devices[deviceID]; exists {
				device.UploadingBytes = 0
				successCount++
			}
		}
	}

	s.logger.Info("uploaded datacap batch: total=", len(records), " success=", successCount, " failed=", failCount)
	return nil
}

func (s *DeviceUsageStore) cleanupStaleDevices() {
	s.mu.Lock()
	defer s.mu.Unlock()

	staleThreshold := s.opts.CacheTTL
	if staleThreshold < time.Hour {
		staleThreshold = time.Hour
	}

	var devicesToRemove []string
	for deviceID, device := range s.devices {
		if device.PendingBytes == 0 && device.UploadingBytes == 0 {
			lastActive := device.LastActivityTime
			if lastActive.IsZero() {
				lastActive = device.LastSyncTime
			}
			if !lastActive.IsZero() && time.Since(lastActive) > staleThreshold {
				devicesToRemove = append(devicesToRemove, deviceID)
			}
		}
	}

	for _, deviceID := range devicesToRemove {
		delete(s.devices, deviceID)
	}
}

func (s *DeviceUsageStore) syncDeviceState(ctx context.Context, deviceID string) error {
	resp, err := s.api.SyncDeviceState(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("failed to sync device state: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var pendingBytes, uploadingBytes int64
	var lastActivityTime time.Time
	var localCountryCode, localPlatform string
	if existing, exists := s.devices[deviceID]; exists {
		pendingBytes = existing.PendingBytes
		uploadingBytes = existing.UploadingBytes
		lastActivityTime = existing.LastActivityTime
		localCountryCode = existing.CountryCode
		localPlatform = existing.Platform
	}

	countryCode := resp.CountryCode
	if countryCode == "" && localCountryCode != "" {
		countryCode = localCountryCode
	}
	platform := resp.Platform
	if platform == "" && localPlatform != "" {
		platform = localPlatform
	}

	s.devices[deviceID] = &DeviceUsage{
		DeviceID:         deviceID,
		PendingBytes:     pendingBytes,
		UploadingBytes:   uploadingBytes,
		BytesUsed:        resp.BytesUsed + pendingBytes + uploadingBytes,
		CapLimit:         resp.CapLimit,
		ExpiryTime:       resp.ExpiryTime,
		CountryCode:      countryCode,
		Platform:         platform,
		LastSyncTime:     time.Now(),
		LastActivityTime: lastActivityTime,
	}

	return nil
}
