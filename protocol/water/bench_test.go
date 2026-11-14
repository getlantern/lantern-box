//go:build integration

package water

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	_ "github.com/refraction-networking/water/transport/v1"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	singlog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/option"
)

// TestMain silences water/wazero runtime logs so they don't corrupt benchstat
// output. Both the stdlib log package and slog.Default() are redirected to
// discard; this applies to both benchmarks and integration tests in this
// package.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// startBenchHTTPServer is the *testing.B counterpart to startTestHTTPServer.
func startBenchHTTPServer(b *testing.B) *httptest.Server {
	b.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shadowsocks_client.wasm" {
			w.Header().Set("Content-Type", "application/wasm")
			data, err := os.ReadFile("testdata/shadowsocks_client.wasm")
			if err != nil {
				http.Error(w, "file not found", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Success!"))
		}
	}))
	b.Cleanup(server.Close)
	return server
}

// startBenchBoxServer is the *testing.B counterpart to startBoxServer.
func startBenchBoxServer(b *testing.B, opts string) (*box.Box, context.Context) {
	b.Helper()
	ctx := box.Context(
		context.Background(),
		include.InboundRegistry(),
		include.OutboundRegistry(),
		include.EndpointRegistry(),
		include.DNSTransportRegistry(),
		include.ServiceRegistry(),
	)

	options, err := json.UnmarshalExtendedContext[O.Options](ctx, []byte(opts))
	if err != nil {
		b.Fatalf("failed to unmarshal box options: %v", err)
	}

	bx, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		b.Fatalf("failed to create box: %v", err)
	}
	if err := bx.Start(); err != nil {
		b.Fatalf("failed to start box: %v", err)
	}
	b.Cleanup(func() { bx.Close() })
	return bx, ctx
}

// newBenchOutbound creates a WATER Outbound for benchmarks. The WASM is
// downloaded from httpServer; the shadowsocks inbound must be running on ssPort.
func newBenchOutbound(b *testing.B, httpServer *httptest.Server, boxCtx context.Context, ssPort uint16) *Outbound {
	b.Helper()
	surl, _ := url.Parse(httpServer.URL)
	transportConfig := map[string]any{
		"remote_addr":          surl.Hostname(),
		"remote_port":          surl.Port(),
		"password":             "8JCsPssfgS8tiRwiMlhARg==",
		"method":               "chacha20-ietf-poly1305",
		"internal_buffer_size": 16383,
	}
	tmp := b.TempDir()
	opts := option.WATEROutboundOptions{
		DownloadTimeout: "30s",
		Dir:             tmp,
		WASMAvailableAt: []string{httpServer.URL + "/shadowsocks_client.wasm"},
		Transport:       "shadowsocks",
		ServerOptions:   O.ServerOptions{Server: "127.0.0.1", ServerPort: ssPort},
		DialerOptions:   O.DialerOptions{},
		Config:          transportConfig,
		SkipHandshake:   true,
		Hashsum:         "05b820148199ea64a7f3e716adefb4c247c8ea0f69175a3af61c36260773e273",
	}

	out, err := NewOutbound(boxCtx, nil, singlog.NewNOPFactory().Logger(), "bench", opts)
	if err != nil {
		b.Fatalf("failed to create outbound: %v", err)
	}
	return out.(*Outbound)
}

// mustPort converts a decimal port string to uint16.
func mustPort(b *testing.B, portStr string) uint16 {
	b.Helper()
	p, err := strconv.Atoi(portStr)
	if err != nil {
		b.Fatalf("invalid port %q: %v", portStr, err)
	}
	return uint16(p)
}

// startEchoServer starts a TCP server that echoes every received byte back.
func startEchoServer(b *testing.B) *net.TCPAddr {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to listen for echo server: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	b.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr)
}

// startSinkServer starts a TCP server that discards all received bytes.
func startSinkServer(b *testing.B) *net.TCPAddr {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to listen for sink server: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c)
			}(conn)
		}
	}()
	b.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr)
}

const ssPassword = "8JCsPssfgS8tiRwiMlhARg=="

func shadowsocksInboundConfig(port int) string {
	return `{
		"log": { "level": "error", "output": "stderr" },
		"inbounds": [{
			"type": "shadowsocks",
			"tag": "ss-in",
			"listen": "127.0.0.1",
			"listen_port": ` + strconv.Itoa(port) + `,
			"method": "chacha20-ietf-poly1305",
			"password": "` + ssPassword + `",
			"network": "tcp"
		}]
	}`
}

// BenchmarkConnectionSetup measures how long it takes to establish a single
// WATER connection (WASM dialer instantiation + shadowsocks handshake) after
// the WASM binary has already been compiled and cached.
func BenchmarkConnectionSetup(b *testing.B) {
	const ssPort = 9480
	httpSrv := startBenchHTTPServer(b)
	_, boxCtx := startBenchBoxServer(b, shadowsocksInboundConfig(ssPort))
	out := newBenchOutbound(b, httpSrv, boxCtx, ssPort)

	surl, _ := url.Parse(httpSrv.URL)
	dest := metadata.ParseSocksaddrHostPort(surl.Hostname(), mustPort(b, surl.Port()))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := out.DialContext(boxCtx, "tcp", dest)
		require.NoError(b, err, "DialContext failed")
		conn.Close()

		// Each DialContext instantiates a new wazero WASM module. Without an
		// explicit GC the instances accumulate and OOM the process. Pausing
		// the timer means GC time is excluded from the ns/op measurement.
		b.StopTimer()
		runtime.GC()
		b.StartTimer()
	}
}

// BenchmarkThroughputRead measures downstream (read) throughput through an
// established WATER + shadowsocks tunnel. A single connection is opened once;
// each iteration sends a 32 KiB chunk and reads it back through a TCP echo
// server. This avoids per-iteration WASM instantiation overhead so the number
// reflects tunnel data-path performance, not connection setup cost.
func BenchmarkThroughputRead(b *testing.B) {
	const ssPort = 9481
	echoAddr := startEchoServer(b)
	httpSrv := startBenchHTTPServer(b)
	_, boxCtx := startBenchBoxServer(b, shadowsocksInboundConfig(ssPort))
	out := newBenchOutbound(b, httpSrv, boxCtx, ssPort)

	dest := metadata.ParseSocksaddrHostPort(echoAddr.IP.String(), uint16(echoAddr.Port))

	conn, err := out.DialContext(boxCtx, "tcp", dest)
	require.NoError(b, err, "DialContext failed")
	defer conn.Close()

	const chunkSize = 32 * 1024
	payload := make([]byte, chunkSize)
	readBuf := make([]byte, chunkSize)

	b.SetBytes(chunkSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err := conn.Write(payload)
		require.NoError(b, err, "Write failed")
		_, err = io.ReadFull(conn, readBuf)
		require.NoError(b, err, "ReadFull failed")
	}
}

// BenchmarkThroughputWrite measures upstream (write) throughput through an
// established WATER + shadowsocks tunnel. A single connection is opened once;
// each iteration writes a 32 KiB chunk into a TCP sink server (which discards
// all bytes). This isolates write-path performance from connection setup cost.
func BenchmarkThroughputWrite(b *testing.B) {
	const ssPort = 9482
	sinkAddr := startSinkServer(b)
	httpSrv := startBenchHTTPServer(b)
	_, boxCtx := startBenchBoxServer(b, shadowsocksInboundConfig(ssPort))
	out := newBenchOutbound(b, httpSrv, boxCtx, ssPort)

	dest := metadata.ParseSocksaddrHostPort(sinkAddr.IP.String(), uint16(sinkAddr.Port))

	conn, err := out.DialContext(boxCtx, "tcp", dest)
	require.NoError(b, err)
	defer conn.Close()

	const chunkSize = 32 * 1024
	payload := make([]byte, chunkSize)

	b.SetBytes(chunkSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, err := conn.Write(payload)
		require.NoError(b, err)
	}
}

// BenchmarkConcurrentDial measures connection-setup throughput under concurrent
// goroutine pressure. The Outbound's internal dialMutex serialises WASM dialer
// creation, so this benchmark also captures mutex-contention overhead.
func BenchmarkConcurrentDial(b *testing.B) {
	const ssPort = 9483
	httpSrv := startBenchHTTPServer(b)
	_, boxCtx := startBenchBoxServer(b, shadowsocksInboundConfig(ssPort))
	out := newBenchOutbound(b, httpSrv, boxCtx, ssPort)

	surl, _ := url.Parse(httpSrv.URL)
	dest := metadata.ParseSocksaddrHostPort(surl.Hostname(), mustPort(b, surl.Port()))

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := out.DialContext(boxCtx, "tcp", dest)
			require.NoError(b, err)
			conn.Close()
		}
	})
}
