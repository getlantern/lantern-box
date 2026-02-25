package main

import (
	"context"
	"fmt"

	box "github.com/sagernet/sing-box"
	"github.com/spf13/cobra"
)

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := cmd.Flags().GetString("config")
		if err != nil {
			return fmt.Errorf("get config flag: %w", err)
		}
		if err := check(path); err != nil {
			return fmt.Errorf("configuration check failed: %w", err)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(checkCmd)
	checkCmd.Flags().String("config", "config.json", "Configuration file path")
}

func check(configPath string) error {
	options, err := readConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(globalCtx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	if err == nil {
		instance.Close()
	}
	cancel()
	return err
}
