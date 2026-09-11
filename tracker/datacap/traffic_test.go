package datacap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"
)

func TestTrafficClassifier(t *testing.T) {
	classify := newTrafficClassifier([]string{"api.custom.example"}, []string{"probe.custom.example"})
	for host, want := range map[string]string{
		"api.iantem.io": "lantern", "API.GETIANTEM.ORG.": "lantern", "api.custom.example": "lantern",
		"www.gstatic.com": "probe", "probe.custom.example": "probe", "news.example": "other",
		"api.iantem.io.evil.example": "other", "fonts.gstatic.com": "other",
		"": "unknown", "192.0.2.1": "unknown", "2001:db8::1": "unknown", "https://api.iantem.io/path": "unknown",
	} {
		require.Equal(t, want, classify(host), host)
	}
}

func TestTrafficReportsBidirectionalOnceAndOmitsDestinations(t *testing.T) {
	var reports []DataCapReport
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "news.example")
		var report DataCapReport
		require.NoError(t, json.Unmarshal(body, &report))
		reports = append(reports, report)
		if len(reports) == 2 {
			http.Error(w, "retry", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"throttle":false}`))
	}))
	defer srv.Close()
	tracker, err := NewDatacapTracker(Options{URL: srv.URL, TrafficCategories: true, ReportInterval: "1h"}, noopLogger)
	require.NoError(t, err)
	ctx := clientcontext.ContextWithClientInfo(context.Background(), clientcontext.ClientInfo{DeviceID: "device", CountryCode: "IR", Platform: "android"})
	conn := tracker.RoutedConnection(ctx, newMockConn([]byte("read")), adapter.InboundContext{Destination: M.ParseSocksaddr("news.example:443")}, nil, nil).(*Conn)
	defer conn.Close()
	_, err = conn.Read(make([]byte, 4))
	require.NoError(t, err)
	conn.sendReport()
	_, err = conn.Write([]byte("ok"))
	require.NoError(t, err)
	conn.sendReport() // rejected; restore the bytes and retain pending connection evidence
	conn.sendReport()
	_, err = conn.Write([]byte("!"))
	require.NoError(t, err)
	conn.sendReport()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, reports, 4)
	require.EqualValues(t, 4, reports[0].TrafficUsage.OtherBytes)
	require.Zero(t, reports[0].TrafficUsage.OtherConnections)
	for _, i := range []int{1, 2} {
		require.EqualValues(t, 2, reports[i].TrafficUsage.OtherBytes)
		require.EqualValues(t, 1, reports[i].TrafficUsage.OtherConnections)
	}
	require.EqualValues(t, 1, reports[3].TrafficUsage.OtherBytes)
	require.Zero(t, reports[3].TrafficUsage.OtherConnections)
}

func TestTrafficCollectionDefaultsOff(t *testing.T) {
	tracker, err := NewDatacapTracker(Options{URL: "http://127.0.0.1:1"}, noopLogger)
	require.NoError(t, err)
	require.Nil(t, tracker.classify)
	encoded, err := json.Marshal(DataCapReport{DeviceID: "device", BytesUsed: 123, TrafficUsage: trafficReport("", 123, true)})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "trafficUsage")
	// UDP associations have no inferred bidirectional TCP connection count.
	unknown := trafficReport("unknown", 123, false)
	require.EqualValues(t, 123, unknown.UnknownBytes)
	require.Zero(t, unknown.UnknownConnections)
}
