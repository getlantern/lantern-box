package metrics

import (
	"context"
	"net"
	"strings"
	"testing"
	"testing/synctest"

	sdkotel "go.opentelemetry.io/otel"

	"github.com/getlantern/geo"
	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader := metric.NewManualReader()
		provider := metric.NewMeterProvider(metric.WithReader(reader))
		sdkotel.SetMeterProvider(provider)

		SetupMetricsManager(geo.NoLookup{})

		ctx := context.Background()
		metricsTracker := NewTracker(ctx)
		defer metricsTracker.Close()

		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		serverTracked := metricsTracker.RoutedConnection(ctx, server, adapter.InboundContext{}, nil, nil)

		clientSentMessage := []byte("A client sent a short request...")
		serverReceive := 0
		go func() {
			buf := make([]byte, len(clientSentMessage))
			var err error
			serverReceive, err = serverTracked.Read(buf)
			assert.NoError(t, err)
		}()

		_, err := client.Write(clientSentMessage)
		assert.NoError(t, err)

		serverSentMessage := []byte("...and the server sent a short response.")
		go func() {
			buf := make([]byte, len(serverSentMessage))
			_, err := client.Read(buf)
			assert.NoError(t, err)
		}()

		serverTransmit, err := serverTracked.Write(serverSentMessage)
		assert.NoError(t, err)

		synctest.Wait()

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
		assert.Equal(t, int64(serverTransmit), results["transmit"], "transmit bytes did not match")
		assert.Equal(t, int64(serverReceive), results["receive"], "receive bytes did not match")
	})
}

func extractCountersByAttribute(rm metricdata.ResourceMetrics, name string) map[string]int64 {
	result := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				result[string(dp.Attributes.Encoded(attribute.DefaultEncoder()))] = dp.Value
			}
		}
	}
	return result
}
