package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/getlantern/geo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/otel"
	"github.com/getlantern/lantern-box/tracker/clientcontext"
	"github.com/getlantern/lantern-box/tracker/metrics"
)

func TestTelemetryE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test (requires docker)")
	}

	ctx := context.Background()

	// --- Start OTEL collector ---
	cfgPath, err := filepath.Abs("testdata/config.yaml")
	require.NoError(t, err)

	ctr, err := testcontainers.GenericContainer(ctx,
		testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "otel/opentelemetry-collector-contrib:0.144.0",
				ExposedPorts: []string{"4318/tcp"},
				WaitingFor: wait.ForLog("Everything is ready").
					WithStartupTimeout(30 * time.Second),
				Files: []testcontainers.ContainerFile{
					{
						HostFilePath:      cfgPath,
						ContainerFilePath: "/etc/otelcol-contrib/config.yaml",
						FileMode:          0o644,
					},
				},
			},
			Started: true,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	port, err := ctr.MappedPort(ctx, "4318")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("http://localhost:%s", port.Port())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	shutdownMeter, err := otel.InitGlobalMeterProvider()
	require.NoError(t, err)

	shutdownTracer, err := otel.InitGlobalTracerProvider()
	require.NoError(t, err)

	metrics.SetupMetricsManager(geo.NoLookup{}, "")

	// --- Start upstream HTTP server ---
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// --- Start sing-box server with trackers ---
	boxCtx := box.BaseContext()
	logger := log.NewNOPFactory().NewLogger("")

	mgr := clientcontext.NewManager(
		clientcontext.MatchBounds{Inbound: []string{"any"}, Outbound: []string{"any"}},
		logger,
	)
	metricsTracker := metrics.NewTracker(boxCtx)

	var serverPort uint16
	startBoxOnFreePortsWith(t, boxCtx, func(port func(string) uint16) option.Options {
		serverPort = port("tcp")
		opts := getOptions(boxCtx, t, "testdata/http_server.json")
		opts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = serverPort
		return opts
	}, func(b *sbox.Box) {
		b.Router().AppendTracker(mgr)
		b.Router().AppendTracker(metricsTracker)
	})

	// --- Start sing-box client with injector ---
	cInfo := clientcontext.ClientInfo{
		DeviceID:    "e2e-device",
		Platform:    "linux",
		IsPro:       true,
		CountryCode: "US",
		Version:     "1.0.0",
	}
	clientCtx := box.BaseContext()
	injector := clientcontext.NewInjector(
		func() clientcontext.ClientInfo { return cInfo },
		"http-out", "socks-out",
	)
	require.NoError(t, injector.Install(clientCtx))

	var clientOpts option.Options
	startBoxOnFreePorts(t, clientCtx, func(port func(string) uint16) option.Options {
		clientOpts = getOptions(clientCtx, t, "testdata/http_client.json")
		clientOpts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = port("tcp")
		for _, ob := range clientOpts.Outbounds {
			switch opts := ob.Options.(type) {
			case *option.HTTPOutboundOptions:
				opts.ServerPort = serverPort
			case *option.SOCKSOutboundOptions:
				opts.ServerPort = serverPort
			}
		}
		return clientOpts
	})

	// --- Send traffic through the proxy ---
	proxyAddr := getProxyAddress(clientOpts.Inbounds)
	require.NotEmpty(t, proxyAddr)

	proxyURL, _ := url.Parse("http://" + proxyAddr)
	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	resp, err := httpClient.Get(upstream.URL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// flush
	shutdownTracer()
	shutdownMeter()

	// Poll collector logs until expected output appears.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		logs, err := ctr.Logs(ctx)
		if !assert.NoError(c, err) {
			return
		}
		defer logs.Close()
		buf, err := io.ReadAll(logs)
		if !assert.NoError(c, err) {
			return
		}
		output := string(buf)
		assert.Contains(c, output, "device_id.connected",
			"should contain device connected span")
		assert.Contains(c, output, "e2e-device",
			"should contain the device ID")
		assert.Contains(c, output, "lantern-box",
			"should contain service name")
		assert.Contains(c, output, "proxy.io",
			"should contain proxy IO metric")
		assert.Contains(c, output, "sing.connections",
			"should contain connections metric")
	}, 10*time.Second, 500*time.Millisecond)
}

func getOptions(ctx context.Context, t *testing.T, path string) option.Options {
	buf, err := os.ReadFile(path)
	require.NoError(t, err)
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, buf)
	require.NoError(t, err)
	return opts
}

func freePort(t testing.TB) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return uint16(port)
}

func TestStartBoxOnFreePortsRetriesTakenPort(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer taken.Close()
	takenPort := uint16(taken.Addr().(*net.TCPAddr).Port)

	ctx := box.BaseContext()
	var builds int
	var bound uint16
	startBoxOnFreePorts(t, ctx, func(port func(string) uint16) option.Options {
		builds++
		bound = port("tcp")
		if builds == 1 {
			bound = takenPort
		}
		opts, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(fmt.Sprintf(
			`{"log":{"disabled":true},"inbounds":[{"type":"mixed","listen":"127.0.0.1","listen_port":%d}]}`, bound)))
		require.NoError(t, err)
		return opts
	})
	require.Equal(t, 2, builds, "a start that lost its port must build again")
	require.NotEqual(t, takenPort, bound)
}

func freeUDPPort(t testing.TB) uint16 {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer c.Close()
	return uint16(c.LocalAddr().(*net.UDPAddr).Port)
}

// startBoxOnFreePorts starts the box build returns and closes it at cleanup.
// build takes its listen ports from port, which returns a free port on network
// ("tcp" or "udp") per call. Another socket can bind a port after port returns
// it, so a start that fails with EADDRINUSE runs build again for new ports.
func startBoxOnFreePorts(t testing.TB, ctx context.Context, build func(port func(network string) uint16) option.Options) *sbox.Box {
	t.Helper()
	return startBoxOnFreePortsWith(t, ctx, build, nil)
}

// startBoxOnFreePortsWith is startBoxOnFreePorts with prepare, if not nil,
// called on each box before it starts.
func startBoxOnFreePortsWith(t testing.TB, ctx context.Context,
	build func(port func(network string) uint16) option.Options, prepare func(*sbox.Box)) *sbox.Box {
	t.Helper()
	port := func(network string) uint16 {
		switch network {
		case "tcp":
			return freePort(t)
		case "udp":
			return freeUDPPort(t)
		}
		t.Fatalf("no free port for network %q", network)
		return 0
	}
	const attempts = 5
	for i := 1; ; i++ {
		b, err := sbox.New(sbox.Options{Context: ctx, Options: build(port)})
		require.NoError(t, err)
		if prepare != nil {
			prepare(b)
		}
		err = b.Start()
		if err == nil {
			t.Cleanup(func() { b.Close() })
			return b
		}
		if !errors.Is(err, syscall.EADDRINUSE) || i == attempts {
			require.NoError(t, err)
		}
	}
}

func getProxyAddress(inbounds []option.Inbound) string {
	for _, inbound := range inbounds {
		if inbound.Tag == "http-client" {
			if opts, ok := inbound.Options.(*option.HTTPMixedInboundOptions); ok {
				return fmt.Sprintf("%s:%v", netip.Addr(*opts.Listen).String(), opts.ListenPort)
			}
		}
	}
	return ""
}
