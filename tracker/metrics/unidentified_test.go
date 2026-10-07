package metrics

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	N "github.com/sagernet/sing/common/network"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchange writes req from the client side of a routed pipe and resp from the
// server side, reading both fully through routed.
func exchange(t *testing.T, client, routed net.Conn, req, resp []byte) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := client.Write(req)
		assert.NoError(t, err)
		_, err = io.ReadFull(client, make([]byte, len(resp)))
		assert.NoError(t, err)
	})
	_, err := io.ReadFull(routed, make([]byte, len(req)))
	require.NoError(t, err)
	_, err = routed.Write(resp)
	require.NoError(t, err)
	wg.Wait()
}

// sumByDirection sums an int64 counter's points by their direction attribute.
func sumByDirection(rm metricdata.ResourceMetrics, name string) map[string]int64 {
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				d, _ := dp.Attributes.Value("network.io.direction")
				out[d.AsString()] += dp.Value
			}
		}
	}
	return out
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// A connection without client info is relayed without reaching proxy.io, so
// its connection and bytes are counted in the unidentified metrics instead,
// with the track as a queryable point attribute.
func TestUnidentifiedConnectionIsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	sdkotel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	SetupMetricsManager(geo.NoLookup{}, "ss2022-triangle-test")
	tracker := NewTracker(context.Background())
	defer tracker.Close()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	metadata := adapter.InboundContext{Inbound: "shadowsocks-in", InboundType: "shadowsocks", Protocol: "tls"}
	routed := tracker.RoutedConnection(context.Background(), server, metadata, nil, nil)
	exchange(t, client, routed, []byte("request"), []byte("a longer response"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	assert.Equal(t, map[string]int64{"receive": 7, "transmit": 17}, sumByDirection(rm, "proxy.unidentified.io"))
	conns := pointAttrs(t, rm, "proxy.unidentified.connections")
	assert.Equal(t, "ss2022-triangle-test", conns["track"])
	assert.Equal(t, "shadowsocks-in", conns["proxy.inbound"])
	assert.Equal(t, "shadowsocks", conns["proxy.inbound_type"])
	assert.NotContains(t, conns, "network.io.direction", "a connection count has no direction")
	assert.False(t, hasMetric(rm, "proxy.io"), "an unidentified connection never reaches proxy.io")
}

// An identified connection is counted in proxy.io as before and never in the
// unidentified metrics.
func TestIdentifiedConnectionIsNotCountedAsUnidentified(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	sdkotel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	SetupMetricsManager(geo.NoLookup{}, "")
	tracker := NewTracker(context.Background())
	defer tracker.Close()

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	routed := tracker.RoutedConnection(context.Background(), infoConn{Conn: server, info: testInfo}, adapter.InboundContext{}, nil, nil)
	exchange(t, client, routed, []byte("request"), []byte("response"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	assert.False(t, hasMetric(rm, "proxy.unidentified.connections"))
	assert.False(t, hasMetric(rm, "proxy.unidentified.io"))
}

// halfCloser records CloseWrite calls on the connection beneath a wrapper.
type halfCloser struct {
	net.Conn
	closedWrite int
}

func (c *halfCloser) CloseWrite() error {
	c.closedWrite++
	return nil
}

// Wrapping must not change half-close: a connection that supports it is still
// reached through the wrapper, and one that does not is not presented as if it
// did, which would turn a full close at EOF into a no-op half-close.
func TestUnidentifiedConnKeepsHalfCloseOfTheWrappedConn(t *testing.T) {
	tracker := NewTracker(context.Background())
	defer tracker.Close()
	_, server := net.Pipe()
	defer server.Close()

	plain := tracker.RoutedConnection(context.Background(), server, adapter.InboundContext{}, nil, nil)
	_, ok := common.Cast[N.WriteCloser](plain)
	assert.False(t, ok, "a conn without half-close must not gain one")

	hc := &halfCloser{Conn: server}
	wrapped := tracker.RoutedConnection(context.Background(), hc, adapter.InboundContext{}, nil, nil)
	require.NoError(t, N.CloseWrite(wrapped))
	assert.Equal(t, 1, hc.closedWrite)
}
