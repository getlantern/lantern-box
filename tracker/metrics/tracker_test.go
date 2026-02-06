package metrics

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stretchr/testify/assert"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func TestConnTracker(t *testing.T) {
	var wg sync.WaitGroup
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	sdkotel.SetMeterProvider(provider)

	SetupMetricsManager("", "")

	metricsTracker, err := NewTracker()
	assert.NoError(t, err)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	ctx := context.Background()
	serverTracked := metricsTracker.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

	clientSentMessage := []byte("A client sent a short request...")
	wg.Add(1)
	serverReceive := 0
	go func() {
		defer wg.Done()
		buf := make([]byte, len(clientSentMessage))
		serverReceive, err = serverTracked.Read(buf)
		assert.NoError(t, err)
	}()

	_, err = client.Write(clientSentMessage)
	assert.NoError(t, err)

	serverSentMessage := []byte("...and the server sent a short response.")
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, len(serverSentMessage))
		_, err = client.Read(buf)
		assert.NoError(t, err)
	}()

	serverTransmit, err := serverTracked.Write(serverSentMessage)
	assert.NoError(t, err)

	wg.Wait()
	var rm metricdata.ResourceMetrics
	reader.Collect(ctx, &rm)

	ioCounter := extractCountersByAttribute(rm, "proxy.io")
	results := map[string]int64{}
	for k, v := range ioCounter {
		if strings.Contains(k, "direction=transmit") {
			results["transmit"] += v
		} else if strings.Contains(k, "direction=receive") {
			results["receive"] += v
		}
	}
	for k, v := range results {
		t.Logf("%s: %d\n", k, v)
	}
	if results["transmit"] != int64(serverTransmit) {
		t.Errorf("transmit bytes did not match, got %d, want %d", results["transmit"], serverTransmit)
	}
	if results["receive"] != int64(serverReceive) {
		t.Errorf("receive bytes did not match, got %d, want %d", results["receive"], serverReceive)
	}
}

func TestPacketConnTracker(t *testing.T) {
	var wg sync.WaitGroup
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	sdkotel.SetMeterProvider(provider)
	SetupMetricsManager("", "")

	metricsTracker, err := NewTracker()
	assert.NoError(t, err)

	client, server := packetPipe()
	defer client.Close()
	defer server.Close()

	ctx := context.Background()
	serverTracked := metricsTracker.RoutedPacketConnection(ctx, server, adapter.InboundContext{}, nil, nil)

	wg.Add(1)
	serverReceive := 0
	go func() {
		defer wg.Done()
		buffer := buf.New()
		_, err = serverTracked.ReadPacket(buffer)
		assert.NoError(t, err)
		serverReceive = buffer.Len()
		t.Logf("Server received '%v'\n", string(buffer.Bytes()))
	}()

	clientSentMessage := []byte("A client sent a short request...")
	clientSendBuf := buf.NewSize(len(clientSentMessage))
	clientSendBuf.Write(clientSentMessage)
	err = client.WritePacket(clientSendBuf, server.LocalAddr().(M.Socksaddr))
	assert.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := buf.New()
		_, err = client.ReadPacket(buffer)
		assert.NoError(t, err)
		t.Logf("Client received '%v'\n", string(buffer.Bytes()))

	}()

	serverSentMessage := []byte("...and the server sent a short response.")
	serverSendBuf := buf.NewSize(len(serverSentMessage))
	serverSendBuf.Write(serverSentMessage)
	serverTransmit := serverSendBuf.Len()
	err = serverTracked.WritePacket(serverSendBuf, client.LocalAddr().(M.Socksaddr))
	assert.NoError(t, err)

	wg.Wait()
	var rm metricdata.ResourceMetrics
	reader.Collect(ctx, &rm)

	ioCounter := extractCountersByAttribute(rm, "proxy.io")
	results := map[string]int64{}
	for k, v := range ioCounter {
		if strings.Contains(k, "direction=transmit") {
			results["transmit"] += v
		} else if strings.Contains(k, "direction=receive") {
			results["receive"] += v
		}
	}
	for k, v := range results {
		t.Logf("%s: %d\n", k, v)
	}
	if results["transmit"] != int64(serverTransmit) {
		t.Errorf("transmit bytes did not match, got %d, want %d", results["transmit"], serverTransmit)
	}
	if results["receive"] != int64(serverReceive) {
		t.Errorf("receive bytes did not match, got %d, want %d", results["receive"], serverReceive)
	}
}

func extractCountersByAttribute(rm metricdata.ResourceMetrics, name string) map[string]int64 {
	result := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				sum := m.Data.(metricdata.Sum[int64])
				for _, dp := range sum.DataPoints {
					key := dp.Attributes.Encoded(attribute.DefaultEncoder())
					result[string(key)] = dp.Value
				}
			}
		}
	}
	return result
}

// Mock packet pipe connection for testing
type packet struct {
	buffer *buf.Buffer
	dest   M.Socksaddr
}

var _ N.PacketConn = (*packetPipeConn)(nil)

type packetPipeConn struct {
	readCh  chan packet
	writeCh chan packet
	once    sync.Once
	done    chan struct{}
}

func packetPipe() (N.PacketConn, N.PacketConn) {
	ch1 := make(chan packet, 16)
	ch2 := make(chan packet, 16)
	done1 := make(chan struct{})
	done2 := make(chan struct{})

	c1 := &packetPipeConn{readCh: ch1, writeCh: ch2, done: done1}
	c2 := &packetPipeConn{readCh: ch2, writeCh: ch1, done: done2}
	return c1, c2
}

func (c *packetPipeConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	select {
	case p := <-c.readCh:
		_, err = buffer.ReadOnceFrom(p.buffer)
		p.buffer.Release()
		return p.dest, err
	case <-c.done:
		return M.Socksaddr{}, net.ErrClosed
	}
}

func (c *packetPipeConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	newBuf := buf.NewSize(buffer.Len())
	newBuf.Write(buffer.Bytes())
	buffer.Release()
	select {
	case c.writeCh <- packet{buffer: newBuf, dest: destination}:
		return nil
	case <-c.done:
		newBuf.Release()
		return net.ErrClosed
	}
}

func (c *packetPipeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *packetPipeConn) LocalAddr() net.Addr                { return M.Socksaddr{} }
func (c *packetPipeConn) SetDeadline(t time.Time) error      { return nil }
func (c *packetPipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *packetPipeConn) SetWriteDeadline(t time.Time) error { return nil }
