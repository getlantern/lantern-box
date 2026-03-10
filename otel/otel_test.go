package otel

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "no env vars",
			want: false,
		},
		{
			name: "OTEL_EXPORTER_OTLP_ENDPOINT",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"},
			want: true,
		},
		{
			name: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318"},
			want: true,
		},
		{
			name: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
			env:  map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4318"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			assert.Equal(t, tt.want, Enabled())
		})
	}
}

func TestBuildResource(t *testing.T) {
	t.Run("default service name", func(t *testing.T) {
		r := buildResource()
		var found bool
		for _, attr := range r.Attributes() {
			if attr.Key == semconv.ServiceNameKey {
				assert.Equal(t, "lantern-box", attr.Value.AsString())
				found = true
			}
		}
		assert.True(t, found, "service.name attribute not found")
	})

	t.Run("OTEL_SERVICE_NAME overrides default", func(t *testing.T) {
		t.Setenv("OTEL_SERVICE_NAME", "custom-svc")
		r := buildResource()
		var name string
		for _, attr := range r.Attributes() {
			if attr.Key == semconv.ServiceNameKey {
				name = attr.Value.AsString()
			}
		}
		assert.Equal(t, "custom-svc", name)
	})

	t.Run("OTEL_RESOURCE_ATTRIBUTES adds attributes", func(t *testing.T) {
		t.Setenv(
			"OTEL_RESOURCE_ATTRIBUTES",
			"proxy.name=test-proxy,deployment.env=staging",
		)
		r := buildResource()
		attrs := make(map[string]string)
		for _, attr := range r.Attributes() {
			attrs[string(attr.Key)] = attr.Value.AsString()
		}
		assert.Equal(t, "test-proxy", attrs["proxy.name"])
		assert.Equal(t, "staging", attrs["deployment.env"])
		assert.Equal(t, "lantern-box", attrs["service.name"])
	})

	t.Run("extra attrs are included", func(t *testing.T) {
		r := buildResource(
			attribute.String("proxy.name", "test-proxy"),
			attribute.Bool("pro", true),
		)
		m := make(map[attribute.Key]attribute.Value)
		for _, attr := range r.Attributes() {
			m[attr.Key] = attr.Value
		}
		assert.Equal(t, "test-proxy", m["proxy.name"].AsString())
		assert.Equal(t, true, m["pro"].AsBool())
		assert.Equal(t, "lantern-box", m["service.name"].AsString())
	})
}

func TestInitTelemetry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	cfgPath, err := filepath.Abs("testdata/config.yaml")
	require.NoError(t, err)

	ctr, err := testcontainers.GenericContainer(ctx,
		testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "otel/opentelemetry-collector-contrib:latest",
				ExposedPorts: []string{"4318/tcp"},
				WaitingFor: wait.ForLog("Everything is ready").
					WithStartupTimeout(30 * time.Second),
				Files: []testcontainers.ContainerFile{
					{
						HostFilePath:      cfgPath,
						ContainerFilePath: "/etc/otelcol-contrib/config.yaml",
						FileMode:          0o644,
					},
				},
			},
			Started: true,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	port, err := ctr.MappedPort(ctx, "4318")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://localhost:%s", port.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "proxy.name=test-proxy")

	// Init providers.
	shutdownMeter, err := InitGlobalMeterProvider()
	require.NoError(t, err)

	shutdownTracer, err := InitGlobalTracerProvider()
	require.NoError(t, err)

	// Emit a test span.
	_, span := sdkotel.Tracer("test").Start(ctx, "test-span")
	span.End()

	// Record a test metric.
	counter, err := sdkotel.Meter("test").Int64Counter("test.counter")
	require.NoError(t, err)
	counter.Add(ctx, 1)

	// Shutdown flushes pending exports.
	shutdownTracer()
	shutdownMeter()

	// Give the collector a moment to process and log.
	time.Sleep(2 * time.Second)

	logs, err := ctr.Logs(ctx)
	require.NoError(t, err)
	defer logs.Close()

	buf, err := io.ReadAll(logs)
	require.NoError(t, err)
	output := string(buf)

	assert.Contains(t, output, "test-span",
		"collector logs should contain the test span name")
	assert.Contains(t, output, "lantern-box",
		"collector logs should contain the service name")
	assert.Contains(t, output, "proxy.name",
		"collector logs should contain proxy.name attribute")
	assert.Contains(t, output, "test.counter",
		"collector logs should contain the metric name")
}
