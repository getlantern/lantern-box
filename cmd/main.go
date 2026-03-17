package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/geo"
	"github.com/spf13/cobra"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/tracker/metrics"
)

type ProxyInfo struct {
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

	// Default to not report metrics
	sdkotel.SetMeterProvider(noop.NewMeterProvider())

	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return
	}

	geoCityURL, err := cmd.Flags().GetString("geo-city-url")
	if err != nil {
		return
	}

	cityDatabaseName, err := cmd.Flags().GetString("city-database-name")
	if err != nil {
		return
	}

	telemetryEndpoint, err := cmd.Flags().GetString("telemetry-endpoint")
	if err != nil {
		return
	}

	proxyInfoPath := strings.Replace(path, ".json", ".ini", 1)
	proxyInfo, err := readProxyInfoFile(proxyInfoPath)
	if err != nil {
		return
	}

	// TODO: what is the best place to do clean up of otel?
	otelOpts := &otel.Opts{
		Endpoint:         otel.GetTelemetryEndpoint(telemetryEndpoint),
		ProxyName:        proxyInfo.Name,
		IsPro:            proxyInfo.Pro,
		Track:            proxyInfo.Track,
		Provider:         proxyInfo.Provider,
		FrontendProvider: proxyInfo.FrontendProvider,
		ProxyProtocol:    proxyInfo.Protocol,
	}
	if _, err := otel.InitGlobalMeterProvider(otelOpts); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize OTEL meter provider: %v\n", err)
	}

	// Report any crash from a previous run, then set up crash output for
	// this run. Order matters: report first (reads crash.log), then setup
	// (truncates crash.log for the next crash).
	crashDir := filepath.Dir(path)
	otel.ReportPreviousCrash(crashDir, otelOpts)
	if err := otel.SetupCrashOutput(crashDir); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up crash output: %v\n", err)
	}

	geolookup := geo.FromWeb(geoCityURL, cityDatabaseName, 24*time.Hour, cityDatabaseName, geo.CountryCode)
	metrics.SetupMetricsManager(geolookup)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
