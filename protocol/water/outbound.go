package water

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	waterDownloader "github.com/getlantern/lantern-water/downloader"
	waterVC "github.com/getlantern/lantern-water/version_control"
	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	"github.com/tetratelabs/wazero"

	"github.com/getlantern/lantern-box/constant"
	L "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/option"
	waterTransport "github.com/getlantern/lantern-box/transport/water"
)

// RegisterOutbound registers the WATER outbound adapter with the given registry.
func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.WATEROutboundOptions](registry, constant.TypeWATER, NewOutbound)
}

// Outbound represents a WATER outbound adapter.
type Outbound struct {
	outbound.Adapter
	logger                logger.ContextLogger
	serverAddr            string
	skipHandshake         bool
	dialerConfig          *water.Config
	transportModuleConfig map[string]any
	dialMutex             sync.Mutex
}

// NewOutbound creates a new WATER outbound adapter.
func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.WATEROutboundOptions) (adapter.Outbound, error) {
	timeout, err := time.ParseDuration(options.DownloadTimeout)
	if err != nil {
		return nil, err
	}

	if options.Dir == "" {
		return nil, E.New("provided an empty storage directory for WATER files")
	}

	wasmDir := filepath.Join(options.Dir, "wasm_files")
	wazeroCompilationDir := filepath.Join(options.Dir, "wazero_compilation_cache")
	for _, dir := range []string{wasmDir, wazeroCompilationDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	slogLogger := slog.New(L.NewLogHandler(logger))
	vc := waterVC.NewWaterVersionControl(wasmDir, slogLogger)
	d, err := waterDownloader.NewWASMDownloader(options.Hashsum, options.WASMAvailableAt, &http.Client{Timeout: timeout})
	if err != nil {
		return nil, E.New("failed to create WASM downloader", err)
	}

	rc, err := vc.GetWASM(ctx, options.Transport, d)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}
	serverAddr := options.ServerOptions.Build()

	// We're creating the compilation cache dir and setting the global value so during runtime
	// it won't need to create one at a temp directory
	compilationCache, err := wazero.NewCompilationCacheWithDir(wazeroCompilationDir)
	if err != nil {
		return nil, err
	}

	water.SetGlobalCompilationCache(compilationCache)

	cfg := &water.Config{
		TransportModuleBin: b,
		OverrideLogger:     slogLogger,
		NetworkDialerFunc: func(network, address string) (net.Conn, error) {
			return outboundDialer.DialContext(log.ContextWithNewID(ctx), network, serverAddr)
		},
	}

	outbound := &Outbound{
		Adapter:               outbound.NewAdapterWithDialerOptions(constant.TypeWATER, tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:                logger,
		serverAddr:            serverAddr.String(),
		dialerConfig:          cfg,
		transportModuleConfig: options.Config,
		dialMutex:             sync.Mutex{},
		skipHandshake:         options.SkipHandshake,
	}

	return outbound, nil
}

func (o *Outbound) newDialer(ctx context.Context, destination M.Socksaddr) (water.Dialer, error) {
	cfg := o.dialerConfig.Clone()

	if o.transportModuleConfig != nil {
		// currently this is the only way to share the destination with the WATER module.
		addr := destination.AddrPort()
		o.transportModuleConfig["remote_addr"] = addr.Addr().String()
		o.transportModuleConfig["remote_port"] = strconv.Itoa(int(addr.Port()))
		transportModuleConfig, err := json.MarshalContext(ctx, o.transportModuleConfig)
		if err != nil {
			return nil, err
		}

		cfg.TransportModuleConfig = water.TransportModuleConfigFromBytes(transportModuleConfig)
	}

	return water.NewDialerWithContext(ctx, cfg)
}

// DialContext dials a connection to the specified network and destination.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	o.dialMutex.Lock()
	defer o.dialMutex.Unlock()
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination

	dialer, err := o.newDialer(ctx, destination)
	if err != nil {
		return nil, err
	}

	conn, err := dialer.DialContext(context.Background(), network, "localhost:0")
	if err != nil {
		return nil, err
	}

	return waterTransport.NewWATERConnection(conn, destination, o.skipHandshake), nil
}

// ListenPacket is not implemented
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, E.New("not implemented")
}

// Network returns the supported network types for this outbound adapter.
// In this case, it supports only TCP.
func (o *Outbound) Network() []string {
	return []string{network.NetworkTCP}
}
