package datacap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
)

// reportSink is a sidecar stub that records reports and can fail them.
type reportSink struct {
	mu      sync.Mutex
	reports []DataCapReport
	fail    bool
}

func (s *reportSink) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report DataCapReport
		require.NoError(t, json.NewDecoder(r.Body).Decode(&report))
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reports = append(s.reports, report)
		if s.fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"throttle":false}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *reportSink) take() []DataCapReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	reports := s.reports
	s.reports = nil
	return reports
}

func (s *reportSink) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func routeConn(t *testing.T, tracker *DatacapTracker, deviceID string, data []byte) *Conn {
	staged := infoConn{Conn: newMockConn(data), info: clientcontext.ClientInfo{DeviceID: deviceID, CountryCode: "IR", Platform: "android"}}
	conn, ok := tracker.RoutedConnection(context.Background(), staged, adapter.InboundContext{Destination: M.ParseSocksaddr("news.example:443")}, nil, nil).(*Conn)
	require.True(t, ok)
	return conn
}

func byDevice(reports []DataCapReport) map[string]DataCapReport {
	m := make(map[string]DataCapReport, len(reports))
	for _, r := range reports {
		m[r.DeviceID] = r
	}
	return m
}

// A device's connections share one report per flush instead of one each.
func TestReporterAggregatesConnectionsPerDevice(t *testing.T) {
	sink := &reportSink{}
	tracker, err := NewDatacapTracker(Options{URL: sink.server(t).URL, ReportInterval: "1h"}, noopLogger)
	require.NoError(t, err)
	defer tracker.Close()

	for range 5 {
		conn := routeConn(t, tracker, "busy", make([]byte, 100))
		defer conn.Close()
		_, err := conn.Read(make([]byte, 100))
		require.NoError(t, err)
		_, err = conn.Write(make([]byte, 10))
		require.NoError(t, err)
	}
	quiet := routeConn(t, tracker, "quiet", make([]byte, 7))
	defer quiet.Close()
	_, err = quiet.Read(make([]byte, 7))
	require.NoError(t, err)

	tracker.reporter.flush()
	reports := byDevice(sink.take())
	require.Len(t, reports, 2)
	require.EqualValues(t, 5*110, reports["busy"].BytesUsed)
	require.Equal(t, "IR", reports["busy"].CountryCode)
	require.Equal(t, "android", reports["busy"].Platform)
	require.EqualValues(t, 7, reports["quiet"].BytesUsed)

	// Nothing new, nothing reported.
	tracker.reporter.flush()
	require.Empty(t, sink.take())
}

// A closed connection's last bytes go out with the next flush, after which
// the device is dropped; usage the sidecar rejects is kept until accepted.
func TestReporterReportsClosedConnectionsAndRetries(t *testing.T) {
	sink := &reportSink{}
	tracker, err := NewDatacapTracker(Options{URL: sink.server(t).URL, ReportInterval: "1h"}, noopLogger)
	require.NoError(t, err)
	defer tracker.Close()

	conn := routeConn(t, tracker, "device", make([]byte, 64))
	_, err = conn.Read(make([]byte, 64))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	sink.setFail(true)
	tracker.reporter.flush()
	require.Len(t, sink.take(), 1)

	sink.setFail(false)
	tracker.reporter.flush()
	reports := sink.take()
	require.Len(t, reports, 1)
	require.EqualValues(t, 64, reports[0].BytesUsed)

	tracker.reporter.mu.Lock()
	require.Empty(t, tracker.reporter.devices)
	tracker.reporter.mu.Unlock()
}

// A device stays tracked while any of its connections is open.
func TestReporterKeepsDevicesWithOpenConnections(t *testing.T) {
	sink := &reportSink{}
	tracker, err := NewDatacapTracker(Options{URL: sink.server(t).URL, ReportInterval: "1h"}, noopLogger)
	require.NoError(t, err)
	defer tracker.Close()

	open := routeConn(t, tracker, "device", make([]byte, 8))
	defer open.Close()
	closed := routeConn(t, tracker, "device", make([]byte, 8))
	_, err = closed.Read(make([]byte, 8))
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	tracker.reporter.flush()
	require.Len(t, sink.take(), 1)
	_, err = open.Read(make([]byte, 8))
	require.NoError(t, err)
	tracker.reporter.flush()
	reports := sink.take()
	require.Len(t, reports, 1)
	require.EqualValues(t, 8, reports[0].BytesUsed)
}

// The sidecar rejects a category with more connections than bytes, so a
// connection that turns bidirectional after its bytes were reported waits
// for a report carrying bytes in its category.
func TestReporterHoldsConnectionsWithoutBytes(t *testing.T) {
	var d deviceUsage
	d.addBytes(10, "other")
	first := d.take()
	require.EqualValues(t, 10, first.trafficBytes[trafficOther])

	d.addConnection("other")
	d.addBytes(5, "lantern")
	second := d.take().trafficUsage()
	require.EqualValues(t, 5, second.LanternBytes)
	require.Zero(t, second.OtherBytes)
	require.Zero(t, second.OtherConnections)

	d.addBytes(3, "other")
	third := d.take().trafficUsage()
	require.EqualValues(t, 3, third.OtherBytes)
	require.EqualValues(t, 1, third.OtherConnections)
}

// Ending the tracker's context reports pending usage once more.
func TestTrackerContextFlushesOnDone(t *testing.T) {
	sink := &reportSink{}
	ctx, cancel := context.WithCancel(context.Background())
	tracker, err := NewDatacapTracker(Options{URL: sink.server(t).URL, ReportInterval: "1h", Context: ctx}, noopLogger)
	require.NoError(t, err)

	conn := routeConn(t, tracker, "device", make([]byte, 32))
	defer conn.Close()
	_, err = conn.Read(make([]byte, 32))
	require.NoError(t, err)

	cancel()
	select {
	case <-tracker.reporter.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reporter did not stop")
	}
	reports := sink.take()
	require.Len(t, reports, 1)
	require.EqualValues(t, 32, reports[0].BytesUsed)
}
