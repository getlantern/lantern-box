package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/getlantern/geo"
	semconv "github.com/getlantern/semconv"
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
	}

	var attrs []attribute.KeyValue
	// Attempt to read proxy info, but this is merely informational,
	// and may not be needed for every subcommand.
	proxyInfoPath, _ := cmd.Flags().GetString("proxy-info")
	if proxyInfoPath != "" {
		if info, err := readProxyInfo(proxyInfoPath); err != nil {
			log.Warn("could not read proxy info, skipping attribute addition: ", err)
		} else {
			attrs = info.resourceAttrs()
		}
	} else {
		log.Info("no proxy-info path provided, skipping attribute addition")
	}

	meterShutdown, err := otel.InitGlobalMeterProvider(attrs...)
	if err != nil {
		log.Error("init meter provider: ", err)
		return
	}
	otelShutdownFuncs = append(otelShutdownFuncs, meterShutdown)

	tracerShutdown, err := otel.InitGlobalTracerProvider(attrs...)
	if err != nil {
		log.Error("init tracer provider: ", err)
		return
	}
	otelShutdownFuncs = append(otelShutdownFuncs, tracerShutdown)

	for _, attr := range attrs {
		log.Info("reporting with attribute: ", fmt.Sprintf("%s=%v", attr.Key, attr.Value.AsString()))
	}

	// Report any crash from a previous run, then set up crash output
	// for this run. Order matters: report first (reads crash.log),
	// then setup (truncates crash.log for the next crash).
	configPath, _ := cmd.Flags().GetString("config")
	if configPath != "" {
		crashDir := filepath.Dir(configPath)
		otel.ReportPreviousCrash(crashDir, attrs...)
		if err := otel.SetupCrashOutput(crashDir); err != nil {
			log.Error("set up crash output: ", err)
		}
	}

	geoCityURL, _ := cmd.Flags().GetString("geo-city-url")
	cityDatabaseName, _ := cmd.Flags().GetString("city-database-name")
	if geoCityURL != "" && cityDatabaseName != "" {
		geolookup := geo.FromWeb(
			geoCityURL, cityDatabaseName,
			24*time.Hour, cityDatabaseName,
			geo.CountryCode,
		)
		metrics.SetupMetricsManager(geolookup, time.Millisecond)
	}
	if otel.Enabled() {
		log.Info("telemetry enabled")

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
	return []attribute.KeyValue{
		semconv.ProxyNameKey.String(info.Name),
		semconv.ProxyProtocolKey.String(info.Protocol),
		semconv.ProxyTrackKey.String(info.Track),
		semconv.ProxyProviderKey.String(info.Provider),
		semconv.ProxyFrontendProviderKey.String(info.FrontendProvider),
		semconv.ClientIsProKey.Bool(info.Pro),
	}
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
