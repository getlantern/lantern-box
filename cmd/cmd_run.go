package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	runtimeDebug "runtime/debug"
	"syscall"
	"time"

	box "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/spf13/cobra"
	"gopkg.in/ini.v1"
)

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("config", "config.json", "Configuration file path")
	runCmd.Flags().String("geo-city-url", "https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz", "URL for downloading GeoLite2-City database")
	runCmd.Flags().String("city-database-name", "GeoLite2-City.mmdb", "Filename for storing GeoLite2-City database")
	runCmd.Flags().String("telemetry-endpoint", "telemetry.iantem.io:443", "Telemetry endpoint for OpenTelemetry exporter")
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run service",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := cmd.Flags().GetString("config")
		if err != nil {
			return fmt.Errorf("get config flag: %w", err)
		}
		return run(path)
	},
}

func readProxyInfoFile(path string) (*ProxyInfo, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading proxy info file: %w", err)
	}
	var info ProxyInfo
	err = cfg.MapTo(&info)
	if err != nil {
		return nil, fmt.Errorf("mapping proxy info file: %w", err)
	}
	return &info, nil
}

func readConfig(path string) (option.Options, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return option.Options{}, fmt.Errorf("reading config file: %w", err)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, content)
	if err != nil {
		return option.Options{}, fmt.Errorf("parsing config file: %w", err)
	}
	return options, nil
}

func create(configPath string) (*box.Box, context.CancelFunc, error) {
	options, err := readConfig(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}

	ctx, cancel := context.WithCancel(globalCtx)
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create service: %w", err)
	}

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(osSignals)
		close(osSignals)
	}()
	startCtx, finishStart := context.WithCancel(context.Background())
	go func() {
		_, loaded := <-osSignals
		if loaded {
			cancel()
			closeMonitor(startCtx)
		}
	}()
	err = instance.Start()
	finishStart()
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "start service")
	}
	return instance, cancel, nil
}

func closeMonitor(ctx context.Context) {
	time.Sleep(C.FatalStopTimeout)
	select {
	case <-ctx.Done():
		return
	default:
	}
	log.Fatal("sing-box did not close!")
}

func run(configPath string) error {
	log.Info("build info: version=%s, commit=%s", version, commit)
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, err := create(configPath)
		if err != nil {
			return err
		}
		runtimeDebug.FreeOSMemory()
		for {
			osSignal := <-osSignals
			if osSignal == syscall.SIGHUP {
				err = check(configPath)
				if err != nil {
					log.Error(E.Cause(err, "reload service"))
					continue
				}
			}
			cancel()
			closeCtx, closed := context.WithCancel(context.Background())
			go closeMonitor(closeCtx)
			err = instance.Close()
			closed()
			if osSignal != syscall.SIGHUP {
				if err != nil {
					log.Error(E.Cause(err, "sing-box did not closed properly"))
				}
				return nil
			}
			break
		}
	}
}
