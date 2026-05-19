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
	"sync/atomic"
	"time"

	waterDownloader "github.com/getlantern/lantern-water/downloader"
	"github.com/getlantern/lantern-water/seed"
	waterVC "github.com/getlantern/lantern-water/version_control"
	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/conntrack"
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
	ready                 atomic.Bool
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
		mu:                    sync.Mutex{},
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

	// Use the wazero interpreter instead of the JIT compiler.
	//
	// The JIT compiler allocates a large native-code arena at runtime-creation
	// time (typically 5-20 MB of resident memory on arm64) in addition to
	// per-instance WASM linear memory.  In memory-constrained environments
	// such as the macOS or iOS NetworkExtension process (jetsam limit ~50 MB)
	// this pushes resident memory over the limit and the OS kills the process
	// silently, manifesting as a VPN disconnect with no Go error or panic.
	//
	// The interpreter avoids the JIT arena entirely.  WASM execution is
	// slower, but the bottleneck for a proxy is network I/O, not instruction
	// throughput, so the tradeoff is acceptable.
	o.dialerConfig.RuntimeConfig().Interpreter()

	// Disable wazero's CloseOnContextDone behaviour.
	//
	// By default water sets WithCloseOnContextDone(true), which tells wazero to
	// forcibly terminate any in-flight WASM function call the moment the context
	// passed to NewCoreWithContext is cancelled.  Each WATER dial creates a core
	// whose context is derived from the per-dial context.  In sing-box a dial
	// context is cancelled as soon as the dial phase completes (URL tests
	// included), which is well before the established connection is closed.
	// With CloseOnContextDone active, that cancellation kills the wazero
	// runtime while the WASM worker is still running, producing an
	// "input/output error" from the worker goroutine.
	//
	// We disable this and rely instead on conn.Close() to terminate the WASM
	// worker: closing the underlying TCPConnPair causes the worker's blocking
	// read/write calls to return with EOF, exiting the worker cleanly.
	o.dialerConfig.RuntimeConfig().SetCloseOnContextDone(false)
	o.mu.Unlock()

	// Validate and warm the interpreter by doing a lightweight parse of the
	// WASM module before setting ready=true so any module errors surface early.
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

	o.ready.Store(true)

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
// JIT compilation cache.  Either way this surfaces module-load errors early,
// before ready is set to true.  It is best-effort; callers must handle a
// non-nil error gracefully.
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
	if !o.ready.Load() {
		o.mu.Lock()
		loadErr := o.loadErr
		dialerReady := o.dialerConfig != nil
		o.mu.Unlock()
		if loadErr != nil {
			return nil, fmt.Errorf("WATER outbound failed to load: %w", loadErr)
		}
		if !dialerReady {
			return nil, fmt.Errorf("WATER outbound is still loading, not ready to dial")
		}
	}

	cfg := o.dialerConfig.Clone()

	// NetworkDialerFunc is per-dial so it captures ctx; cancelling the dial also cancels the inner TCP connection.
	cfg.NetworkDialerFunc = func(network, address string) (net.Conn, error) {
		conn, err := o.outboundDialer.DialContext(ctx, network, o.serverAddr)
		if err != nil {
			return nil, err
		}
		switch conn := conn.(type) {
		case *conntrack.Conn:
			return conn.Conn, nil
		default:
			return conn, nil
		}
	}

	o.logger.DebugContext(ctx, "building new dialer", slog.String("destination", destination.String()))
	if o.transportModuleConfig != nil {
		// Clone before mutating so concurrent dials don't race on the shared map.
		// WATER's config API provides no other injection point for per-dial parameters.
		merged := make(map[string]any, len(o.transportModuleConfig)+2)
		for k, v := range o.transportModuleConfig {
			merged[k] = v
		}
		merged["remote_addr"] = destination.AddrString()
		merged["remote_port"] = strconv.FormatUint(uint64(destination.Port), 10)
		transportModuleConfig, err := json.MarshalContext(ctx, merged)
		if err != nil {
			return nil, err
		}

		o.logger.DebugContext(ctx, "building transport module config", slog.String("config_json", string(transportModuleConfig)))
		cfg.TransportModuleConfig = water.TransportModuleConfigFromBytes(transportModuleConfig)
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
	// wazero can panic during WASM core creation or execution; convert to error
	// so a single bad outbound doesn't crash the daemon process.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in WATER dial", slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			err = fmt.Errorf("WATER dial panicked: %v", r)
		}
	}()

	dialer, err := o.newDialer(ctx, destination)
	if err != nil {
		o.logger.ErrorContext(ctx, "failed to build new WATER dialer", slog.Any("error", err), slog.String("destination", destination.String()))
		return nil, err
	}

	// wazero's DialContext does not respect context cancellation mid-run, so
	// racing it in a goroutine ensures callers with short deadlines (e.g. URL-test)
	// are not blocked for the full OS TCP timeout when the server is unreachable.
	type dialResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in WATER DialContext", slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				ch <- dialResult{nil, fmt.Errorf("WATER DialContext panicked: %v", r)}
			}
		}()
		conn, err := dialer.DialContext(ctx, N.NetworkTCP, "localhost:0")
		ch <- dialResult{conn, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			o.logger.ErrorContext(ctx, "WATER failed to dial", slog.Any("error", r.err))
			return nil, r.err
		}
		return waterTransport.NewWATERConnection(r.conn, destination, o.skipHandshake), nil
	case <-ctx.Done():
		// Drain the background dial when it finishes to avoid leaving an open
		// server connection unreferenced.
		go func() {
			if r := <-ch; r.err == nil && r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// ListenPacket creates a UoT packet connection through the WATER transport.
func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	return o.uotClient.ListenPacket(ctx, destination)
}
