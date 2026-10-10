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
	// Private builds deliberately keep telemetry and crash reporting local.
	// No OTLP providers, crash uploaders, or official geo services are
	// initialized unless the caller explicitly opts in at runtime.
	sdkotel.SetMeterProvider(noop.NewMeterProvider())
	configPath, _ := cmd.Flags().GetString("config")
	if configPath != "" {
		if err := otel.SetupCrashOutput(filepath.Dir(configPath)); err != nil {
			log.Debug("local crash output unavailable", "error", err)
		}
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
