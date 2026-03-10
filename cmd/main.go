package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/getlantern/geo"
	"go.opentelemetry.io/otel/attribute"
	"gopkg.in/ini.v1"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/tracker/metrics"
	"github.com/sagernet/sing-box/log"
	"github.com/spf13/cobra"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
)

type proxyInfo struct {
	Name             string `ini:"proxyname"`
	Pro              bool   `ini:"pro"`
	Track            string `ini:"track"`
	Provider         string `ini:"provider"`
	FrontendProvider string `ini:"frontend_provider"`
	Protocol         string `ini:"proxyprotocol"`
}

var globalCtx context.Context
var (
	version string
	commit  string
)

var otelShutdownFuncs []func()

var rootCmd = &cobra.Command{
	Use:               "lantern-box",
	Version:           version,
	PersistentPreRun:  preRun,
	CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	SilenceErrors:     true,
	SilenceUsage:      true,
}

func preRun(cmd *cobra.Command, args []string) {
	globalCtx = box.BaseContext()

	// Default to not report metrics.
	sdkotel.SetMeterProvider(noop.NewMeterProvider())

	if !otel.Enabled() {
		log.Info("telemetry disabled (set OTEL_EXPORTER_OTLP_ENDPOINT to enable)")
		return
	}

	// Read proxy info as resource attributes.
	var attrs []attribute.KeyValue
	path, _ := cmd.Flags().GetString("config")
	if path != "" {
		iniPath := strings.Replace(path, ".json", ".ini", 1)
		if info, err := readProxyInfo(iniPath); err != nil {
			log.Warn("could not read proxy info: ", err)
		} else {
			attrs = info.resourceAttrs()
		}
	}

	meterShutdown, err := otel.InitGlobalMeterProvider(attrs...)
	if err != nil {
		log.Warn("failed to init meter provider: ", err)
		return
	}
	otelShutdownFuncs = append(otelShutdownFuncs, meterShutdown)

	tracerShutdown, err := otel.InitGlobalTracerProvider(attrs...)
	if err != nil {
		log.Warn("failed to init tracer provider: ", err)
		return
	}
	otelShutdownFuncs = append(otelShutdownFuncs, tracerShutdown)

	log.Info("telemetry enabled")

	geoCityURL, _ := cmd.Flags().GetString("geo-city-url")
	cityDatabaseName, _ := cmd.Flags().GetString("city-database-name")
	if geoCityURL != "" && cityDatabaseName != "" {
		geolookup := geo.FromWeb(
			geoCityURL, cityDatabaseName,
			24*time.Hour, cityDatabaseName,
			geo.CountryCode,
		)
		metrics.SetupMetricsManager(geolookup)
	}
}

func readProxyInfo(path string) (*proxyInfo, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading proxy info: %w", err)
	}
	var info proxyInfo
	if err := cfg.MapTo(&info); err != nil {
		return nil, fmt.Errorf("mapping proxy info: %w", err)
	}
	return &info, nil
}

func (info *proxyInfo) resourceAttrs() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.Bool("pro", info.Pro),
		attribute.String("proxy.name", info.Name),
		attribute.String("protocol", info.Protocol),
		attribute.String("track", info.Track),
		attribute.String("provider", info.Provider),
		attribute.String("frontend.provider", info.FrontendProvider),
	}
	return attrs
}

func shutdownOtel() {
	for _, shutdown := range otelShutdownFuncs {
		shutdown()
	}
}

func main() {
	defer shutdownOtel()
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
