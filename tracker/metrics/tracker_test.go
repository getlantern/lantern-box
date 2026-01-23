package metrics

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
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
