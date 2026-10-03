package datacap

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// infoConn stages ClientInfo on a connection for InfoFromConn in tests.
type infoConn struct {
	net.Conn
	info clientcontext.ClientInfo
}

func (c infoConn) ClientInfo() (clientcontext.ClientInfo, bool) { return c.info, true }

// infoPacketConn stages ClientInfo on a packet connection for InfoFromConn in
// tests.
type infoPacketConn struct {
	N.PacketConn
	info clientcontext.ClientInfo
}

func (c infoPacketConn) ClientInfo() (clientcontext.ClientInfo, bool) { return c.info, true }

func TestNewMissingURL(t *testing.T) {
	_, err := NewDatacapTracker(Options{URL: ""}, log.NewNOPFactory().Logger())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url not defined")
}

// Data-cap enforcement applies only to identified free users, so every other
// connection is returned unchanged.
func TestSkip(t *testing.T) {
	tests := []struct {
		name string
		info *clientcontext.ClientInfo
	}{
		// Not from a clientcontext-aware client.
		{name: "no info"},
		// Usage and throttling are keyed by device ID, so info without one would
		// pool every such client under a single empty ID.
		{name: "no device ID", info: &clientcontext.ClientInfo{Platform: "test", CountryCode: "US"}},
		// A device ID is set so the skip can only come from IsPro.
		{name: "pro", info: &clientcontext.ClientInfo{DeviceID: "device-pro", IsPro: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker, err := NewDatacapTracker(Options{URL: "http://example.com"}, log.NewNOPFactory().Logger())
			require.NoError(t, err)

			udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
			require.NoError(t, err)
			defer udpConn.Close()
			var conn net.Conn = newMockConn(nil)
			var packetConn N.PacketConn = bufio.NewPacketConn(udpConn)
			if tt.info != nil {
				conn = infoConn{Conn: conn, info: *tt.info}
				packetConn = infoPacketConn{PacketConn: packetConn, info: *tt.info}
			}

			assert.Equal(t, conn, tracker.RoutedConnection(context.Background(), conn, adapter.InboundContext{}, nil, nil))
			assert.Equal(t, packetConn, tracker.RoutedPacketConnection(context.Background(), packetConn, adapter.InboundContext{}, nil, nil))
		})
	}
}

// A free user is throttled only once the datacap server reports the cap is
// exhausted.
func TestThrottle(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     bool
	}{
		{name: "under cap", response: `{"throttle":false, "capLimit": 1000}`},
		{name: "exhausted", response: `{"throttle":true, "remainingBytes": 0, "capLimit": 1000}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tt.response))
			}))
			defer server.Close()

			tracker, err := NewDatacapTracker(Options{URL: server.URL, ReportInterval: "100ms"}, log.NewNOPFactory().Logger())
			require.NoError(t, err)

			staged := infoConn{Conn: newMockConn(make([]byte, 1024)), info: clientcontext.ClientInfo{
				DeviceID:    "device-" + tt.name,
				Platform:    "test",
				CountryCode: "US",
			}}
			conn, ok := tracker.RoutedConnection(context.Background(), staged, adapter.InboundContext{}, nil, nil).(*Conn)
			require.True(t, ok, "a free user's connection must be tracked")
			defer conn.Close()

			_, _ = conn.Read(make([]byte, 10))
			time.Sleep(200 * time.Millisecond)

			require.Equal(t, tt.want, conn.throttler.IsEnabled())
			if tt.want {
				// Downloads (writes) drop to the low tier; uploads (reads) keep the
				// default upload speed.
				assert.Equal(t, int64(lowTierSpeedBytesPerSec), conn.throttler.GetWriteRate())
				assert.Equal(t, int64(defaultUploadSpeedBytesPerSec), conn.throttler.GetReadRate())
			}
		})
	}
}
