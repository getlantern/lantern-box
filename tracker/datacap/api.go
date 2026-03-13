package datacap

import "context"

// ReportSink is the interface for reporting datacap consumption.
// Both the HTTP Client (legacy sidecar mode) and DeviceUsageStore (direct mode) implement this.
type ReportSink interface {
	Report(ctx context.Context, report *DataCapReport) (*DataCapStatus, error)
}

// DataCapAPI abstracts the cloud API for datacap operations.
// The concrete gRPC implementation lives in cmd/ to isolate the proto dependency.
type DataCapAPI interface {
	ReportUsage(ctx context.Context, batch *UsageBatch) (*ReportUsageResult, error)
	SyncDeviceState(ctx context.Context, deviceID string) (*DeviceState, error)
}

// UsageBatch is a batch of usage records to upload to the cloud API.
type UsageBatch struct {
	Records []UsageRecord
}

// UsageRecord is a single device's usage to report.
type UsageRecord struct {
	DeviceID    string
	BytesUsed   int64
	CapLimit    int64
	Platform    string
	CountryCode string
}

// ReportUsageResult contains per-device results from a batch upload.
type ReportUsageResult struct {
	Results []UsageResultEntry
}

// UsageResultEntry is the result of reporting usage for a single device.
type UsageResultEntry struct {
	DeviceID string
	Success  bool
	Error    string
}

// DeviceState is the cloud API's view of a device's datacap state.
type DeviceState struct {
	DeviceID    string
	BytesUsed   int64
	CapLimit    int64
	ExpiryTime  int64
	CountryCode string
	Platform    string
}
