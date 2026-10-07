package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
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
	// The bubble keeps the tracker's report goroutine from outliving the test,
	// which would race the next test's SetupMetricsManager over the package's
	// instruments.
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()

		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))

		assert.True(t, hasMetric(rm, "proxy.io"), "an identified connection is counted in proxy.io")
		assert.False(t, hasMetric(rm, "proxy.unidentified.connections"))
		assert.False(t, hasMetric(rm, "proxy.unidentified.io"))
	})
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

// Wrapping must not change half-close as sing-box's connection manager sees
// it: it half-closes a destination only when the conn itself is an
// N.WriteCloser, so the wrapper must be one exactly when the wrapped conn is.
func TestUnidentifiedConnKeepsHalfCloseOfTheWrappedConn(t *testing.T) {
	tracker := NewTracker(context.Background())
	defer tracker.Close()
	_, server := net.Pipe()
	defer server.Close()

	plain := tracker.RoutedConnection(context.Background(), server, adapter.InboundContext{}, nil, nil)
	_, ok := plain.(N.WriteCloser)
	assert.False(t, ok, "a conn without half-close must not gain one")

	hc := &halfCloser{Conn: server}
	wrapped := tracker.RoutedConnection(context.Background(), hc, adapter.InboundContext{}, nil, nil)
	wc, ok := wrapped.(N.WriteCloser)
	require.True(t, ok, "a conn with half-close must keep it")
	require.NoError(t, wc.CloseWrite())
	assert.Equal(t, 1, hc.closedWrite)
}

// sing's copy loop unwraps counters to the connection beneath, keeping its
// fast paths, and counts the bytes it moves through the returned functions.
func TestUnidentifiedConnUnwrapsForSingCopy(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	sdkotel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	SetupMetricsManager(geo.NoLookup{}, "")
	tracker := NewTracker(context.Background())
	defer tracker.Close()
	_, server := net.Pipe()
	defer server.Close()
	hc := &halfCloser{Conn: server}

	routed := tracker.RoutedConnection(context.Background(), hc, adapter.InboundContext{}, nil, nil)
	r, rxFuncs := N.UnwrapCountReader(routed, nil)
	w, txFuncs := N.UnwrapCountWriter(routed, nil)
	assert.Same(t, hc, r)
	assert.Same(t, hc, w)
	for _, f := range rxFuncs {
		f(100)
	}
	for _, f := range txFuncs {
		f(250)
	}

	packet := tracker.RoutedPacketConnection(context.Background(), fakePacketConn{}, adapter.InboundContext{}, nil, nil)
	pr, prFuncs := N.UnwrapCountPacketReader(packet, nil)
	pw, pwFuncs := N.UnwrapCountPacketWriter(packet, nil)
	assert.Equal(t, fakePacketConn{}, pr)
	assert.Equal(t, fakePacketConn{}, pw)
	for _, f := range prFuncs {
		f(7)
	}
	for _, f := range pwFuncs {
		f(3)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	assert.Equal(t, map[string]int64{"receive": 107, "transmit": 253}, sumByDirection(rm, "proxy.unidentified.io"))
}

// rejectingPacketConn fails every write, releasing the buffer as a writer may.
type rejectingPacketConn struct{ N.PacketConn }

func (rejectingPacketConn) WritePacket(buffer *buf.Buffer, _ M.Socksaddr) error {
	buffer.Release()
	return errors.New("rejected")
}

// A packet the writer rejects was not relayed, so it is not counted.
func TestUnidentifiedPacketConnCountsOnlyWrittenPackets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	sdkotel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	SetupMetricsManager(geo.NoLookup{}, "")
	tracker := NewTracker(context.Background())
	defer tracker.Close()

	packet := tracker.RoutedPacketConnection(context.Background(), rejectingPacketConn{}, adapter.InboundContext{}, nil, nil)
	buffer := buf.New()
	_, err := buffer.Write([]byte("payload"))
	require.NoError(t, err)
	require.Error(t, packet.WritePacket(buffer, M.Socksaddr{}))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	assert.Zero(t, sumByDirection(rm, "proxy.unidentified.io")["transmit"])
}
