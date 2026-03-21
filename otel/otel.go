package otel

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Enabled checks if an OTLP endpoint is configured via standard OTEL_EXPORTER_OTLP_* env vars.
func Enabled() bool {
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

func InitGlobalMeterProvider(attrs ...attribute.KeyValue) (func(), error) {
	exp, err := otlpmetrichttp.New(context.Background(),
		otlpmetrichttp.WithTemporalitySelector(deltaForCounters),
	)
	if err != nil {
		return nil, fmt.Errorf("new meter provider: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(buildResource(attrs...)),
	)
	sdkotel.SetMeterProvider(mp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mp.Shutdown(ctx); err != nil {
			log.Error(E.Cause(err, "shutting down meter provider"))
		}
	}, nil
}

func InitGlobalTracerProvider(attrs ...attribute.KeyValue) (func(), error) {
	exp, err := otlptracehttp.New(context.Background())
	if err != nil {
		return nil, fmt.Errorf("new tracer provider: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(buildResource(attrs...)),
	)
	sdkotel.SetTracerProvider(tp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Error(E.Cause(err, "shutting down tracer provider"))
		}
	}, nil
}

func deltaForCounters(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	switch kind {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindUpDownCounter,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

// buildResource creates an OTEL resource with a default service name
// of "lantern-box". All attributes can be overridden or extended via
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES env vars.
func buildResource(extras ...attribute.KeyValue) *resource.Resource {
	attrs := append([]attribute.KeyValue{
		semconv.ServiceNameKey.String("lantern-box"),
		semconv.ServiceVersionKey.String(vcsRevision()),
	}, extras...)
	r, _ := resource.New(context.Background(),
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
	)
	return r
}

func vcsRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}
