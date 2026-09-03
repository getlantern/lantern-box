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
	semconv "github.com/getlantern/semconv"
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
		otlpmetrichttp.WithTemporalitySelector(deltaTemporality),
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

// deltaTemporality exports every instrument kind with delta temporality,
// including histograms.
//
// Delta is what lets the ops collector aggregate an attribute away. Stripping
// a label from a cumulative stream merges independent monotonic series whose
// resets are interleaved, which corrupts rate() silently rather than failing;
// delta datapoints just sum. The ops collector relies on this to drop
// route.id/instance.id/host.name from proxy.session.goodput, whose ~10.8k
// distinct host.name values were driving the histogram to ~55M series (see
// getlantern/engineering#3831).
//
// Temporality is chosen per instrument KIND at the exporter, so this covers
// every histogram this binary emits, not just goodput. The other one is
// sing.connection_duration, which the ops collector drops outright (no
// readers), so goodput is the only histogram whose temporality is observable
// downstream.
func deltaTemporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.DeltaTemporality
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
