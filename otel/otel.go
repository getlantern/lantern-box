package otel

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type Opts struct {
	Endpoint         string
	Headers          map[string]string
	ProxyName        string
	Track            string
	Provider         string
	DC               string
	FrontendProvider string
	FrontendDC       string
	ProxyProtocol    string
	Addr             string
	IsPro            bool
	Legacy           bool
}

func GetTelemetryEndpoint(fallback string) string {
	if endpoint := os.Getenv("CUSTOM_OTLP_ENDPOINT"); endpoint != "" {
		return endpoint
	}
	return fallback
}

func InitGlobalMeterProvider(opts *Opts) (func(), error) {
	metricOpts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpoint(opts.Endpoint),
		otlpmetrichttp.WithHeaders(opts.Headers),
		otlpmetrichttp.WithTemporalitySelector(func(kind sdkmetric.InstrumentKind) metricdata.Temporality {
			switch kind {
			case
				sdkmetric.InstrumentKindCounter,
				sdkmetric.InstrumentKindUpDownCounter,
				sdkmetric.InstrumentKindObservableCounter,
				sdkmetric.InstrumentKindObservableUpDownCounter:
				return metricdata.DeltaTemporality
			default:
				return metricdata.CumulativeTemporality
			}
		}),
	}

	// If endpoint doesn't use port 443, assume insecure (HTTP not HTTPS)
	if !strings.Contains(opts.Endpoint, ":443") {
		log.Debug("Using insecure connection for OTEL metrics endpoint ", opts.Endpoint)
		metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
	}

	exp, err := otlpmetrichttp.New(context.Background(), metricOpts...)
	if err != nil {
		return nil, err
	}

	// Create a new meter provider
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(opts.buildResource()),
	)

	// Set the meter provider as global
	sdkotel.SetMeterProvider(mp)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := mp.Shutdown(ctx)
		if err != nil {
			log.Error(E.Cause(err, "error shutting down meter provider"))
		}
	}, nil
}

func (opts *Opts) buildResource() *resource.Resource {
	attributes := []attribute.KeyValue{
		semconv.ServiceNameKey.String("lantern-box"),
		attribute.String("protocol", opts.ProxyProtocol),
		attribute.Bool("pro", opts.IsPro),
		attribute.Bool("legacy", opts.Legacy),
	}
	if opts.Track != "" {
		attributes = append(attributes, attribute.String("track", opts.Track))
	}
	if opts.ProxyName != "" {
		log.Debug("Will report with proxy.name ", opts.ProxyName)
		attributes = append(attributes, attribute.String("proxy.name", opts.ProxyName))
	}
	if opts.Provider != "" {
		log.Debug("Will report with provider ", opts.Provider)
		attributes = append(attributes, attribute.String("provider", opts.Provider))
	}
	if opts.DC != "" {
		log.Debug("Will report with dc ", opts.DC)
		attributes = append(attributes, attribute.String("dc", opts.DC))
	}
	if opts.FrontendProvider != "" {
		log.Debug("Will report frontend provider  in dc ", opts.FrontendProvider, opts.FrontendDC)
		attributes = append(attributes, attribute.String("frontend.provider", opts.FrontendProvider))
		attributes = append(attributes, attribute.String("frontend.dc", opts.FrontendDC))
	}
	return resource.NewWithAttributes(semconv.SchemaURL, attributes...)
}
