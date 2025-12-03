package main

import (
	"context"
	"fmt"
	"os"

	box "github.com/sagernet/sing-box"
	"github.com/spf13/cobra"

	"github.com/getlantern/lantern-box/protocol"
)

var globalCtx context.Context

var rootCmd = &cobra.Command{
	Use:               "lantern-box",
	Version:           "v1.11.7",
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
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
