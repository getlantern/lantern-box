package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	box "github.com/getlantern/lantern-box"
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
	globalCtx = box.BaseContext()
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
