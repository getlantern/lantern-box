package water

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	waterDownloader "github.com/getlantern/lantern-water/downloader"
	"github.com/getlantern/lantern-water/seed"
	waterVC "github.com/getlantern/lantern-water/version_control"
	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/sagernet/sing/service"
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
	skipHandshake         bool
	dialerConfig          *water.Config
	loadErr               error
	transportModuleConfig map[string]any
	outboundDialer        N.Dialer
	serverAddr            M.Socksaddr
	mu                    sync.Mutex
	seeder                *seed.Seeder
	cancelLoad            context.CancelFunc
	uotClient             *uot.Client
}

type waterDialer struct {
	outbound *Outbound
}

func (d *waterDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return d.outbound.dialTCP(ctx, destination)
}

func (d *waterDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
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

	// We're creating the compilation cache dir and setting the global value so during runtime
	// it won't need to create one at a temp directory
	compilationCache, err := wazero.NewCompilationCacheWithDir(wazeroCompilationDir)
	if err != nil {
		return nil, err
	}
	water.SetGlobalCompilationCache(compilationCache)

	loadCtx, cancelLoad := context.WithCancel(ctx)

	o := &Outbound{
		Adapter:               outbound.NewAdapterWithDialerOptions(constant.TypeWATER, tag, []string{N.NetworkTCP, N.NetworkUDP}, options.DialerOptions),
		logger:                logger,
		transportModuleConfig: options.Config,
		skipHandshake:         options.SkipHandshake,
		cancelLoad:            cancelLoad,
	}

	uotOptions := common.PtrValueOrDefault(options.UDPOverTCP)
	if uotOptions.Enabled {
		o.uotClient = &uot.Client{
			Dialer:  &waterDialer{outbound: o},
			Version: uotOptions.Version,
		}
	}

	go o.loadConfig(loadCtx, logger, options, timeout, wasmDir)

	return o, nil
}

func (o *Outbound) loadConfig(ctx context.Context, logger log.ContextLogger, options option.WATEROutboundOptions, downloadTimeout time.Duration, wasmDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in while loading water config", slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		}
	}()
	logger.DebugContext(ctx, "loading WATER configuration in background")

	serverAddr := options.ServerOptions.Build()

	slogLogger := slog.New(L.NewLogHandler(logger))
	vc := waterVC.NewWaterVersionControl(wasmDir, slogLogger)
	httpClient := &http.Client{Timeout: downloadTimeout}

	if options.DownloadDetour != "" {
		logger.DebugContext(ctx, "download detour option set", slog.Any("detour", options.DownloadDetour))
		outboundManager := service.FromContext[adapter.OutboundManager](ctx)
		if detourDialer, loaded := outboundManager.Outbound(options.DownloadDetour); loaded {
			httpClient.Transport = &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return detourDialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
				},
			}
			logger.DebugContext(ctx, "using detour outbound for downloading WASM files", slog.Any("detour", options.DownloadDetour))
		}
	}
	d, err := waterDownloader.NewWASMDownloader(options.Hashsum, options.WASMAvailableAt, httpClient)
	if err != nil {
		logger.ErrorContext(ctx, "failed to create WASM downloader", slog.Any("error", err))
		o.mu.Lock()
		o.loadErr = err
		o.mu.Unlock()
		return
	}

	rc, err := vc.GetWASM(ctx, options.Transport, d)
	if err != nil {
		logger.ErrorContext(ctx, "failed to load WASM file", slog.Any("error", err))
		o.mu.Lock()
		o.loadErr = err
		o.mu.Unlock()
		return
	}
	defer rc.Close()

	b, err := io.ReadAll(rc)
	if err != nil {
		logger.ErrorContext(ctx, "failed to read WASM file", slog.Any("error", err))
		o.mu.Lock()
		o.loadErr = err
		o.mu.Unlock()
		return
	}

	logger.DebugContext(ctx, "successfully loaded WASM file", slog.String("transport", options.Transport))

	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		logger.ErrorContext(ctx, "failed to create outbound dialer", slog.Any("error", err))
		o.mu.Lock()
		o.loadErr = err
		o.mu.Unlock()
		return
	}

	o.mu.Lock()
	o.outboundDialer = outboundDialer
	o.serverAddr = serverAddr
	o.dialerConfig = &water.Config{
		TransportModuleBin: b,
		OverrideLogger:     slogLogger,
	}

	// Use the interpreter, not the JIT. The JIT arena (5-20 MB on arm64) pushes
	// the macOS/iOS NetworkExtension past its ~50 MB jetsam limit and the OS
	// kills the process silently. Interpreted WASM is slower, but the proxy
	// bottleneck is network I/O.
	o.dialerConfig.RuntimeConfig().Interpreter()

	// Don't tie the WASM worker's lifetime to the dial ctx. sing-box cancels
	// the dial ctx as soon as the dial phase completes — well before the conn
	// closes — so the default tears down the worker mid-stream, surfacing as
	// "input/output error". Shutdown is driven by conn.Close() instead.
	o.dialerConfig.RuntimeConfig().SetCloseOnContextDone(false)
	o.mu.Unlock()

	// Validate and warm the interpreter by doing a lightweight parse of the
	// WASM module so any module errors surface early.
	logger.DebugContext(ctx, "validating WASM module via interpreter",
		slog.String("transport", options.Transport))
	if preErr := preCompileWASM(ctx, b, o.dialerConfig); preErr != nil {
		logger.WarnContext(ctx, "WASM module validation failed (non-fatal)",
			slog.String("transport", options.Transport),
			slog.Any("error", preErr))
	} else {
		logger.DebugContext(ctx, "WASM module validation complete",
			slog.String("transport", options.Transport))
	}

	if options.SeedEnabled {
		transportFilepath := filepath.Join(wasmDir, fmt.Sprintf("%s.%s", options.Transport, "wasm"))
		httpClient := &http.Client{}
		if options.SeedDetour != "" {
			logger.DebugContext(ctx, "seeding detour option set", slog.Any("detour", options.SeedDetour))
			outboundManager := service.FromContext[adapter.OutboundManager](ctx)
			if detourDialer, loaded := outboundManager.Outbound(options.SeedDetour); loaded {
				httpClient.Transport = &http.Transport{
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						return detourDialer.DialContext(ctx, network, M.ParseSocksaddr(addr))
					},
				}
				logger.DebugContext(ctx, "using detour outbound for seeding WASM files", slog.Any("detour", options.SeedDetour))
			}
		}
		seeder, err := seed.New(transportFilepath, options.AnnounceList, httpClient)
		if err != nil {
			logger.WarnContext(ctx, "failed to seed WASM", slog.Any("error", err), slog.String("transport", transportFilepath))
		}
		if seeder != nil {
			logger.DebugContext(ctx, "seeding WASM file", slog.String("magnet_uri", seeder.MagnetURI()), slog.String("transport", transportFilepath))
		}
		o.mu.Lock()
		o.seeder = seeder
		o.mu.Unlock()
	}
}

func (o *Outbound) Close() error {
	o.cancelLoad()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seeder != nil {
		return o.seeder.Close()
	}
	return nil
}

// preCompileWASM validates the WASM binary against the provided runtime config
// by compiling it once.  In interpreter mode (the default for WATER dials) this
// is a lightweight parse+validate step; in compiler mode it warms the on-disk
// JIT compilation cache.  Either way this surfaces module-load errors early.
// It is best-effort; callers must handle a non-nil error gracefully.
func preCompileWASM(ctx context.Context, wasmBin []byte, cfg *water.Config) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic during WASM pre-compilation",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("WASM pre-compilation panicked: %v", r)
		}
	}()
	rt := wazero.NewRuntimeWithConfig(ctx, cfg.RuntimeConfig().GetConfig())
	defer rt.Close(ctx)
	_, err = rt.CompileModule(ctx, wasmBin)
	return
}

func (o *Outbound) newDialer(ctx context.Context, destination M.Socksaddr) (water.Dialer, error) {
	o.mu.Lock()
	loadErr := o.loadErr
	dialerCfg := o.dialerConfig
	outboundDialer := o.outboundDialer
	serverAddr := o.serverAddr
	transportModuleConfig := o.transportModuleConfig
	o.mu.Unlock()

	if loadErr != nil {
		return nil, fmt.Errorf("WATER outbound failed to load: %w", loadErr)
	}
	if dialerCfg == nil {
		return nil, fmt.Errorf("WATER outbound is still loading, not ready to dial")
	}

	cfg := dialerCfg.Clone()

	// NetworkDialerFunc is per-dial so it captures ctx; cancelling the dial also cancels the inner TCP connection.
	cfg.NetworkDialerFunc = func(network, address string) (net.Conn, error) {
		conn, err := outboundDialer.DialContext(ctx, network, serverAddr)
		if err != nil {
			return nil, err
		}
		if uConn, ok := conn.(common.WithUpstream); ok {
			if upstreamConn, ok := uConn.Upstream().(net.Conn); ok {
				return upstreamConn, nil
			}
		}
		return conn, nil
	}

	o.logger.DebugContext(ctx, "building new dialer", slog.String("destination", destination.String()))
	if transportModuleConfig != nil {
		// Clone before mutating so concurrent dials don't race on the shared map.
		// WATER's config API provides no other injection point for per-dial parameters.
		merged := make(map[string]any, len(transportModuleConfig)+2)
		for k, v := range transportModuleConfig {
			merged[k] = v
		}
		merged["remote_addr"] = destination.AddrString()
		merged["remote_port"] = strconv.FormatUint(uint64(destination.Port), 10)
		transportModuleConfigJSON, err := json.MarshalContext(ctx, merged)
		if err != nil {
			return nil, err
		}

		o.logger.DebugContext(ctx, "building transport module config", slog.String("config_json", string(transportModuleConfigJSON)))
		cfg.TransportModuleConfig = water.TransportModuleConfigFromBytes(transportModuleConfigJSON)
	}

	return water.NewDialerWithContext(ctx, cfg)
}

// DialContext dials a connection to the specified network and destination.
func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination

	switch N.NetworkName(network) {
	case N.NetworkTCP:
		return o.dialTCP(ctx, destination)
	case N.NetworkUDP:
		return o.uotClient.DialContext(ctx, network, destination)
	}
	return nil, E.New("unsupported network: ", network)
}

func (o *Outbound) dialTCP(ctx context.Context, destination M.Socksaddr) (conn net.Conn, err error) {
	var rawConn net.Conn
	// wazero can panic during WASM core creation or execution; convert to error
	// so a single bad outbound doesn't crash the daemon process.
	defer func() {
		if r := recover(); r != nil {
			if rawConn != nil {
				rawConn.Close()
			}
			slog.Error("panic in WATER dial", slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("WATER dial panicked: %v", r)
		}
	}()

	dialer, err := o.newDialer(ctx, destination)
	if err != nil {
		o.logger.ErrorContext(ctx, "failed to build new WATER dialer", slog.Any("error", err), slog.String("destination", destination.String()))
		return nil, err
	}

	// NetworkDialerFunc (called inside dialer.DialContext during Initialize/_start)
	// uses the caller's ctx, so context cancellation unblocks the dial naturally.
	rawConn, err = dialer.DialContext(ctx, N.NetworkTCP, "localhost:0")
	if err != nil {
		o.logger.ErrorContext(ctx, "WATER failed to dial", slog.Any("error", err))
		return nil, err
	}
	return &asyncCloseConn{Conn: waterTransport.NewWATERConnection(rawConn, destination, o.skipHandshake)}, nil
}

// asyncCloseConn wraps a net.Conn whose Close can block (e.g. WATER's
// WaitWorker); dispatching to a goroutine prevents stalling callers on the
// synchronous cancellation path.
type asyncCloseConn struct {
	net.Conn
	closeOnce sync.Once
}

func (c *asyncCloseConn) Close() error {
	c.closeOnce.Do(func() { go c.Conn.Close() })
	return nil
}

// ListenPacket creates a UoT packet connection through the WATER transport.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	return o.uotClient.ListenPacket(ctx, destination)
}
