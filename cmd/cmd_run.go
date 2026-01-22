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
	"github.com/sagernet/sing/service"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"gopkg.in/ini.v1"

	"github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/metrics"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/getlantern/lantern-box/tracker/datacap"
)

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("config", "config.json", "Configuration file path")
	runCmd.Flags().String("geo-city-url", "https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz", "URL for downloading GeoLite2-City database")
	runCmd.Flags().String("city-database-name", "GeoLite2-City.mmdb", "Filename for storing GeoLite2-City database")
	runCmd.Flags().String("telemetry-endpoint", "telemetry.iantem.io:443", "Telemetry endpoint for OpenTelemetry exporter")
	runCmd.Flags().String("datacap-url", "", "Datacap server URL")
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run service",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := cmd.Flags().GetString("config")
		if err != nil {
			return fmt.Errorf("get config flag: %w", err)
		}
		datacapURL, err := cmd.Flags().GetString("datacap-url")
		if err != nil {
			return fmt.Errorf("get datacap-url flag: %w", err)
		}
		return run(path, datacapURL)
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

func create(configPath string, datacapURL string) (*box.Box, context.CancelFunc, error) {
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

	if datacapURL != "" {
		// Add datacap tracker
		log.Info("Datacap enabled. Creating trackers...")
		clientCtxMgr := clientcontext.NewManager(clientcontext.MatchBounds{
			Inbound:  []string{""},
			Outbound: []string{""},
		}, log.StdLogger())
		instance.Router().AppendTracker(clientCtxMgr)
		service.MustRegister[adapter.ClientContextManager](ctx, clientCtxMgr)

		datacapTracker, err := datacap.NewDatacapTracker(
			datacap.Options{
				URL:            datacapURL,
				ReportInterval: "10s",
			},
			log.StdLogger(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("create datacap tracker: %w", err)
		}
		clientCtxMgr.AppendTracker(datacapTracker)
	} else {
		log.Warn("Datacap URL not provided, datacap tracking disabled")
	}

	mp := otel.GetMeterProvider()
	if _, ok := mp.(noop.MeterProvider); ok {
		log.Info("Metrics not enabled, no meter provider configured")
	} else {
		metricsTracker, err := metrics.NewTracker()
		if err != nil {
			return nil, nil, fmt.Errorf("create metrics tracker: %w", err)
		}
		instance.Router().AppendTracker(metricsTracker)
		log.Info("Metric Tracking Enabled")
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

func run(configPath string, datacapURL string) error {
	log.Info("build info: version ", version, ", commit ", commit)
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, err := create(configPath, datacapURL)
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
