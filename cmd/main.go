package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/tracker/metrics"
	"github.com/sagernet/sing-box/log"
	"github.com/spf13/cobra"
	sdkotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
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
		log.Warn("telemetry disabled: could not read proxy info file: ", err)
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

	otel.InitGlobalMeterProvider(otelOpts)
	otel.InitGlobalTracerProvider(otelOpts)
	log.Info("telemetry enabled, exporting to ", otelOpts.Endpoint)

	metrics.SetupMetricsManager(geoCityURL, cityDatabaseName)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
