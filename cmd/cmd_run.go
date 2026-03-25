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

	"github.com/getlantern/lantern-box/adapter"
	lbotel "github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/getlantern/lantern-box/tracker/datacap"
	"github.com/getlantern/lantern-box/tracker/metrics"
)

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("config", "config.json", "Configuration file path")
	runCmd.Flags().String("geo-city-url", "https://lanterngeo.lantern.io/GeoLite2-City.mmdb.tar.gz", "URL for downloading GeoLite2-City database")
	runCmd.Flags().String("city-database-name", "GeoLite2-City.mmdb", "Filename for storing GeoLite2-City database")
	runCmd.Flags().String("datacap-url", "", "Datacap sidecar URL (legacy mode)")
	runCmd.Flags().String("datacap-grpc-api", "", "Datacap cloud gRPC API address (direct mode, mutually exclusive with --datacap-url)")
	runCmd.Flags().Duration("datacap-batch-interval", 30*time.Second, "Datacap batch upload interval (direct mode)")
	runCmd.Flags().Duration("datacap-cache-ttl", 5*time.Minute, "Datacap device state cache TTL (direct mode)")
	runCmd.Flags().String("datacap-ca-cert", "", "Path to CA cert for datacap mTLS")
	runCmd.Flags().String("datacap-client-cert", "", "Path to client cert for datacap mTLS")
	runCmd.Flags().String("datacap-client-key", "", "Path to client key for datacap mTLS")
	runCmd.Flags().String("proxy-info", "", "Path to proxy info INI file")
}

type datacapFlags struct {
	URL            string
	GRPCAPI        string
	CACertPath     string
	ClientCertPath string
	ClientKeyPath  string
	BatchInterval  time.Duration
	CacheTTL       time.Duration
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run service",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := cmd.Flags().GetString("config")
		if err != nil {
			return fmt.Errorf("get config flag: %w", err)
		}
		dcFlags := datacapFlags{}
		if dcFlags.URL, err = cmd.Flags().GetString("datacap-url"); err != nil {
			return fmt.Errorf("get datacap-url flag: %w", err)
		}
		if dcFlags.GRPCAPI, err = cmd.Flags().GetString("datacap-grpc-api"); err != nil {
			return fmt.Errorf("get datacap-grpc-api flag: %w", err)
		}
		if dcFlags.GRPCAPI == "" {
			dcFlags.GRPCAPI = os.Getenv("DATACAP_GRPC_API")
		}
		if dcFlags.CACertPath, err = cmd.Flags().GetString("datacap-ca-cert"); err != nil {
			return fmt.Errorf("get datacap-ca-cert flag: %w", err)
		}
		if dcFlags.ClientCertPath, err = cmd.Flags().GetString("datacap-client-cert"); err != nil {
			return fmt.Errorf("get datacap-client-cert flag: %w", err)
		}
		if dcFlags.ClientKeyPath, err = cmd.Flags().GetString("datacap-client-key"); err != nil {
			return fmt.Errorf("get datacap-client-key flag: %w", err)
		}
		if dcFlags.BatchInterval, err = cmd.Flags().GetDuration("datacap-batch-interval"); err != nil {
			return fmt.Errorf("get datacap-batch-interval flag: %w", err)
		}
		if dcFlags.CacheTTL, err = cmd.Flags().GetDuration("datacap-cache-ttl"); err != nil {
			return fmt.Errorf("get datacap-cache-ttl flag: %w", err)
		}

		if dcFlags.URL != "" && dcFlags.GRPCAPI != "" {
			return fmt.Errorf("--datacap-url and --datacap-grpc-api are mutually exclusive")
		}

		return run(path, dcFlags)
	},
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

func create(configPath string, dcFlags datacapFlags) (*box.Box, context.CancelFunc, error) {
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
		cancel()
		return nil, nil, fmt.Errorf("create service: %w", err)
	}

	clientCtxMgr := clientcontext.NewManager(clientcontext.MatchBounds{
		Inbound:  []string{""},
		Outbound: []string{""},
	}, log.StdLogger())
	instance.Router().AppendTracker(clientCtxMgr)
	service.MustRegister[adapter.ClientContextManager](ctx, clientCtxMgr)

	if lbotel.Enabled() {
		metricsTracker := metrics.NewTracker(ctx)
		clientCtxMgr.AppendTracker(metricsTracker)
		log.Info("Metric Tracking Enabled")
	}

	if dcFlags.GRPCAPI != "" {
		log.Info("Datacap direct mode enabled (gRPC API: ", dcFlags.GRPCAPI, ")")
		var mtls *datacapMTLSConfig
		if dcFlags.ClientCertPath != "" {
			mtls = &datacapMTLSConfig{
				CACertPath:     dcFlags.CACertPath,
				ClientCertPath: dcFlags.ClientCertPath,
				ClientKeyPath:  dcFlags.ClientKeyPath,
			}
		}
		grpcConn, err := newDataCapGRPCConn(dcFlags.GRPCAPI, mtls)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("create datacap gRPC connection: %w", err)
		}
		api := newGRPCDataCapAPI(grpcConn)
		store := datacap.NewDeviceUsageStore(api, datacap.StoreOptions{
			BatchInterval: dcFlags.BatchInterval,
			CacheTTL:      dcFlags.CacheTTL,
		}, log.StdLogger())
		tracker := datacap.NewDatacapTrackerWithStore(store, 10*time.Second, log.StdLogger())
		tracker.Start(ctx)
		clientCtxMgr.AppendTracker(tracker)
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			tracker.Stop(shutdownCtx)
			grpcConn.Close()
		}()
	} else if dcFlags.URL != "" {
		log.Info("Datacap sidecar mode enabled (URL: ", dcFlags.URL, ")")
		datacapTracker, err := datacap.NewDatacapTracker(
			datacap.Options{
				URL:            dcFlags.URL,
				ReportInterval: "10s",
			},
			log.StdLogger(),
		)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("create datacap tracker: %w", err)
		}
		clientCtxMgr.AppendTracker(datacapTracker)
	} else {
		log.Warn("Datacap not configured, datacap tracking disabled")
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

func run(configPath string, dcFlags datacapFlags) error {
	log.Info("build info: version ", version, ", commit ", commit)
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, err := create(configPath, dcFlags)
		if err != nil {
			return err
		}
		runtimeDebug.FreeOSMemory()
		for {
			osSignal := <-osSignals
			log.Info("received signal: ", osSignal)
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
