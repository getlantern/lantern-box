package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/protocol"
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
	version   string
	commit    string
	buildDate string
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
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	globalCtx = box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return
	}

	proxyInfoPath := strings.Replace(path, ".json", ".ini", 1)

	proxyInfo, err := readProxyInfoFile(proxyInfoPath)
	if err != nil {
		return
	}

	otel.InitGlobalMeterProvider(&otel.Opts{
		Endpoint:         otel.GetTelemetryEndpoint(),
		ProxyName:        proxyInfo.Name,
		IsPro:            proxyInfo.Pro,
		Track:            proxyInfo.Track,
		Provider:         proxyInfo.Provider,
		FrontendProvider: proxyInfo.FrontendProvider,
		ProxyProtocol:    proxyInfo.Protocol,
	})
	globalCtx = box.BaseContext()
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
