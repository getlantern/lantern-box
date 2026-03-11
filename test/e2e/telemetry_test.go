package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/sagernet/sing/service"

	box "github.com/getlantern/lantern-box"
	"github.com/getlantern/lantern-box/adapter"
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
				Image:        "otel/opentelemetry-collector-contrib:latest",
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

	metrics.SetupMetricsManager(geo.NoLookup{})

	// --- Start upstream HTTP server ---
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// --- Start sing-box server with trackers ---
	boxCtx := box.BaseContext()
	logger := log.NewNOPFactory().NewLogger("")

	serverPort := freePort(t)
	clientPort := freePort(t)
	serverOpts := getOptions(boxCtx, t, "testdata/http_server.json")
	clientOpts := getOptions(boxCtx, t, "testdata/http_client.json")
	patchPorts(&serverOpts, &clientOpts, serverPort, clientPort)

	serverBox, err := sbox.New(sbox.Options{Context: boxCtx, Options: serverOpts})
	require.NoError(t, err)

	mgr := clientcontext.NewManager(
		clientcontext.MatchBounds{Inbound: []string{"any"}, Outbound: []string{"any"}},
		logger,
	)
	serverBox.Router().AppendTracker(mgr)
	service.MustRegister[adapter.ClientContextManager](boxCtx, mgr)

	metricsTracker := metrics.NewTracker(boxCtx)
	serverBox.Router().AppendTracker(metricsTracker)

	require.NoError(t, serverBox.Start())
	defer serverBox.Close()

	// --- Start sing-box client with injector ---
	cInfo := clientcontext.ClientInfo{
		DeviceID:    "e2e-device",
		Platform:    "linux",
		IsPro:       true,
		CountryCode: "US",
		Version:     "1.0.0",
	}
	injector := clientcontext.NewClientContextInjector(
		func() clientcontext.ClientInfo { return cInfo },
		clientcontext.MatchBounds{Inbound: []string{"any"}, Outbound: []string{"any"}},
	)

	clientBox, err := sbox.New(sbox.Options{Context: boxCtx, Options: clientOpts})
	require.NoError(t, err)
	clientBox.Router().AppendTracker(injector)
	require.NoError(t, clientBox.Start())
	defer clientBox.Close()

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

	time.Sleep(2 * time.Second)

	// check logs
	logs, err := ctr.Logs(ctx)
	require.NoError(t, err)
	defer logs.Close()

	buf, err := io.ReadAll(logs)
	require.NoError(t, err)
	output := string(buf)

	assert.Contains(t, output, "device_id.connected",
		"should contain device connected span")
	assert.Contains(t, output, "e2e-device",
		"should contain the device ID")
	assert.Contains(t, output, "lantern-box",
		"should contain service name")
	assert.Contains(t, output, "proxy.io",
		"should contain proxy IO metric")
	assert.Contains(t, output, "sing.connections",
		"should contain connections metric")
}

func getOptions(ctx context.Context, t *testing.T, path string) option.Options {
	buf, err := os.ReadFile(path)
	require.NoError(t, err)
	opts, err := json.UnmarshalExtendedContext[option.Options](ctx, buf)
	require.NoError(t, err)
	return opts
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return uint16(port)
}

func patchPorts(
	serverOpts *option.Options,
	clientOpts *option.Options,
	serverPort, clientPort uint16,
) {
	serverOpts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = serverPort
	clientOpts.Inbounds[0].Options.(*option.HTTPMixedInboundOptions).ListenPort = clientPort
	for _, ob := range clientOpts.Outbounds {
		switch opts := ob.Options.(type) {
		case *option.HTTPOutboundOptions:
			opts.ServerPort = serverPort
		case *option.SOCKSOutboundOptions:
			opts.ServerPort = serverPort
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
