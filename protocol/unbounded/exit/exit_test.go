//go:build with_quic

package exit

import (
	"bytes"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/quic-go"
	"github.com/sagernet/quic-go/http3"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/tls"
	sblog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/json/badoption"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// placeholderConn is a connected UDP socket as clientcore's ICE packet conn: one peer, and a
// fixed address in place of the real one.
type placeholderConn struct{ *net.UDPConn }

var placeholderAddr = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}

func (c placeholderConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.UDPConn.Read(p)
	return n, placeholderAddr, err
}

func (c placeholderConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.UDPConn.Write(p) }

// pathPair returns two packet conns joined to each other, as the two ends of an ICE path.
func pathPair(t *testing.T) (volunteer, user placeholderConn) {
	t.Helper()
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	_ = a.Close()
	_ = b.Close()
	va, err := net.DialUDP("udp4", a.LocalAddr().(*net.UDPAddr), b.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	vb, err := net.DialUDP("udp4", b.LocalAddr().(*net.UDPAddr), a.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = va.Close(); _ = vb.Close() })
	return placeholderConn{va}, placeholderConn{vb}
}

// dialRouter routes each TCP connection by dialing dial (or its destination) and relaying,
// and records what it was handed. Adapted from samizdatreverse's bridge_test.go.
type dialRouter struct {
	dial string // where to send TCP; empty means the destination itself

	mu     sync.Mutex
	routed []adapter.InboundContext
}

func (r *dialRouter) RouteConnection(context.Context, net.Conn, adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *dialRouter) RoutePacketConnection(context.Context, N.PacketConn, adapter.InboundContext) error {
	return errors.New("not used")
}

func (r *dialRouter) record(md adapter.InboundContext) {
	r.mu.Lock()
	r.routed = append(r.routed, md)
	r.mu.Unlock()
}

func (r *dialRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.record(md)
	go func() {
		addr := r.dial
		if addr == "" {
			addr = md.Destination.String()
		}
		up, err := net.Dial("tcp", addr)
		if err != nil {
			_ = N.CloseOnHandshakeFailure(conn, onClose, err)
			return
		}
		_ = N.ReportHandshakeSuccess(conn)
		err = bufio.CopyConn(ctx, conn, up)
		_ = conn.Close()
		_ = up.Close()
		onClose(err)
	}()
}

func (r *dialRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.record(md)
	_ = N.CloseOnHandshakeFailure(conn, onClose, errors.New("not used"))
}

func (r *dialRouter) lastRouted() adapter.InboundContext {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routed) == 0 {
		return adapter.InboundContext{}
	}
	return r.routed[len(r.routed)-1]
}

func (r *dialRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.routed)
}

// newEchoRouter routes TCP to a loopback echo listener.
func newEchoRouter(t *testing.T) *dialRouter {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return &dialRouter{dial: l.Addr().String()}
}

// refuseRouter refuses every connection as a reject rule does.
type refuseRouter struct{ dialRouter }

func (r *refuseRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, md adapter.InboundContext, onClose N.CloseHandlerFunc) {
	r.record(md)
	_ = N.CloseOnHandshakeFailure(conn, onClose, &rule.RejectedError{Cause: errors.New("refused " + md.Destination.String())})
}

// alwaysReady is a router's ready check whose rule sets always hold rules.
func alwaysReady() error { return nil }

func newTestExit(t *testing.T) *Exit {
	t.Helper()
	e, err := New(context.Background(), sblog.NewNOPFactory().Logger())
	require.NoError(t, err)
	t.Cleanup(e.Close)
	return e
}

func readyExit(t *testing.T, r adapter.ConnectionRouterEx) *Exit {
	t.Helper()
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, r, time.Now().Add(time.Hour), alwaysReady)
	return e
}

// serve runs the exit's Serve on conn for a pool session until the test ends, and returns a
// channel closed when Serve returns.
func serve(t *testing.T, e *Exit, conn net.PacketConn, password string) <-chan struct{} {
	t.Helper()
	return serveKind(t, e, conn, password, Pool)
}

// serveKind is serve for a session of kind.
func serveKind(t *testing.T, e *Exit, conn net.PacketConn, password string, kind Kind) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Options().Serve(ctx, conn, password, kind == Personal); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return done
}

// oneConnDialer hands the hysteria2 client the user's end of the path.
type oneConnDialer struct{ conn net.Conn }

func (d oneConnDialer) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return d.conn, nil
}

func (d oneConnDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("not used")
}

type clientOpts struct{ sendBPS, receiveBPS uint64 }

func testClient(t *testing.T, conn placeholderConn, pin, password string, opts ...clientOpts) *hysteria2.Client {
	t.Helper()
	pinBytes, err := base64.StdEncoding.DecodeString(pin)
	require.NoError(t, err)
	lg := sblog.NewNOPFactory().Logger()
	tlsConfig, err := tls.NewClient(context.Background(), lg, "", option.OutboundTLSOptions{
		Enabled:                    true,
		ServerName:                 "exit.invalid",
		ALPN:                       badoption.Listable[string]{"h3"},
		CertificatePublicKeySHA256: badoption.Listable[[]byte]{pinBytes},
	})
	require.NoError(t, err)
	var o clientOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	c, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       context.Background(),
		Dialer:        oneConnDialer{conn},
		Logger:        lg,
		ServerAddress: M.SocksaddrFromNet(placeholderAddr),
		SendBPS:       o.sendBPS,
		ReceiveBPS:    o.receiveBPS,
		Password:      password,
		TLSConfig:     tlsConfig,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseWithError(errors.New("test over")) })
	return c
}

func echo(t *testing.T, c *hysteria2.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialConn(ctx, M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
}

func TestExitNotReadyUntilEnabledAndRouted(t *testing.T) {
	e := newTestExit(t)
	o := e.Options()
	assert.False(t, o.Ready(false), "new: off and no router")
	e.SetEnabled(true)
	assert.False(t, o.Ready(false), "no router")
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	assert.True(t, o.Ready(false))
	_, _, ok := o.Params(false)
	assert.True(t, ok)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(-time.Second), alwaysReady)
	assert.False(t, o.Ready(false), "registration expired")
	_, _, ok = o.Params(false)
	assert.False(t, ok)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	e.SetEnabled(false)
	assert.False(t, o.Ready(false), "flag off")
	e.SetEnabled(true)
	assert.True(t, o.Ready(false), "flag back on")
	e.Close()
	assert.False(t, o.Ready(false), "closed")
	e.SetEnabled(true)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	assert.False(t, o.Ready(false), "closed for good")
}

func TestExitOptionsAreStable(t *testing.T) {
	e := newTestExit(t)
	assert.Same(t, e.Options(), e.Options())
}

func TestExitParamsAreFreshPerSession(t *testing.T) {
	e := readyExit(t, &dialRouter{})
	pin1, pw1, ok := e.Options().Params(false)
	require.True(t, ok)
	pin2, pw2, _ := e.Options().Params(false)
	assert.Equal(t, pin1, pin2, "one certificate per process")
	assert.NotEqual(t, pw1, pw2)
	raw, err := base64.RawURLEncoding.DecodeString(pw1)
	require.NoError(t, err)
	assert.Len(t, raw, 32)
}

func TestExitServesTCPThroughTheRouter(t *testing.T) {
	r := newEchoRouter(t)
	e := readyExit(t, r)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	echo(t, testClient(t, uConn, pin, password))
	md := r.lastRouted()
	assert.Equal(t, InboundTag, md.Inbound)
	assert.Equal(t, "unbounded", md.InboundType)
	assert.Equal(t, "example.com:80", md.Destination.String())
	assert.Equal(t, M.SocksaddrFromNet(placeholderAddr).Unwrap(), md.Source, "the session's placeholder, never the user's address")
}

// A destination that answers and closes, as an HTTP/1.0 server does to end its response,
// ends the user's stream too: sing's copy half-closes the stream, which hysteria2's can't.
func TestExitEndsTheStreamWhenTheDestinationCloses(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("bye"))
			_ = c.Close()
		}
	}()
	e := readyExit(t, &dialRouter{dial: l.Addr().String()})
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := testClient(t, uConn, pin, password).DialConn(ctx, M.ParseSocksaddr("example.com:80"))
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// The client sends its request with its first write.
	_, err = conn.Write([]byte("hi"))
	require.NoError(t, err)
	// hysteria2's client wraps the EOF, so io.ReadAll returns it.
	got, err := io.ReadAll(conn)
	require.ErrorIs(t, err, io.EOF, "the stream ends at EOF, not the deadline")
	assert.Equal(t, "bye", string(got))
}

func TestExitRoutesThroughTheCurrentRouter(t *testing.T) {
	r1, r2 := newEchoRouter(t), newEchoRouter(t)
	e := readyExit(t, r1)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	c := testClient(t, uConn, pin, password)
	echo(t, c)
	e.SetRouter(Pool, r2, time.Now().Add(time.Hour), alwaysReady)
	echo(t, c)
	assert.Equal(t, 1, r1.count())
	assert.Equal(t, 1, r2.count(), "a stream opened after rotation goes to the new router")
}

func TestExitRejectsAWrongPasswordOrPin(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		wrongPin, wrongPassword bool
	}{
		{"password", false, true}, {"pin", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEchoRouter(t)
			e := readyExit(t, r)
			vConn, uConn := pathPair(t)
			pin, password, _ := e.Options().Params(false)
			serve(t, e, vConn, password)
			if tc.wrongPassword {
				_, password, _ = e.Options().Params(false) // another session's password
			}
			if tc.wrongPin {
				other, err := newCertificate()
				require.NoError(t, err)
				pin = other.pin
			}
			c := testClient(t, uConn, pin, password)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.DialConn(ctx, M.ParseSocksaddr("example.com:80"))
			assert.Error(t, err)
			assert.Zero(t, r.count(), "nothing reaches the router")
		})
	}
}

func TestExitServeEndsWhenRevoked(t *testing.T) {
	for name, revoke := range map[string]func(*Exit){
		"disabled":       func(e *Exit) { e.SetEnabled(false) },
		"router cleared": func(e *Exit) { e.SetRouter(Pool, nil, time.Time{}, alwaysReady) },
		"closed":         func(e *Exit) { e.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			e := readyExit(t, newEchoRouter(t))
			vConn, _ := pathPair(t)
			_, password, _ := e.Options().Params(false)
			done := serve(t, e, vConn, password)
			revoke(e)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Serve outlived its exit")
			}
		})
	}
}

// A registration that runs out revokes the exit by itself: lantern-cloud no longer vouches for
// the box's router, and the registrar may still be busy getting the next one.
func TestExitExpiresWithItsRegistration(t *testing.T) {
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(500*time.Millisecond), alwaysReady)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	done := serve(t, e, vConn, password)
	echo(t, testClient(t, uConn, pin, password))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve outlived the registration")
	}
	assert.False(t, e.Options().Ready(false))
}

// A stream that reaches the exit after its registration ran out isn't routed, whether or not
// the expiry has revoked it yet.
func TestExitRoutesNothingAfterExpiry(t *testing.T) {
	r := newEchoRouter(t)
	e := readyExit(t, r)
	now := time.Now()
	e.mu.Lock()
	e.now = func() time.Time { return now.Add(2 * time.Hour) }
	e.mu.Unlock()
	assert.Nil(t, e.currentRouter(Pool))
	server, client := net.Pipe()
	defer client.Close()
	closed := make(chan error, 1)
	(&handler{exit: e, kind: Pool}).NewConnectionEx(context.Background(), server, M.Socksaddr{}, M.ParseSocksaddr("example.com:80"),
		func(err error) { closed <- err })
	select {
	case err := <-closed:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was neither routed nor refused")
	}
	assert.Zero(t, r.count(), "nothing reaches the router")
}

// Revocation ends sessions asynchronously, so a stream or flow a session opens in the
// meantime must find no router once the exit is off or closed.
func TestExitRoutesNothingOnceRevoked(t *testing.T) {
	for name, revoke := range map[string]func(*Exit){
		"disabled": func(e *Exit) { e.SetEnabled(false) },
		"closed":   func(e *Exit) { e.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			r := newEchoRouter(t)
			e := readyExit(t, r)
			revoke(e)
			assert.Nil(t, e.currentRouter(Pool))
			server, client := net.Pipe()
			defer client.Close()
			closed := make(chan error, 2)
			h := &handler{exit: e, kind: Pool}
			h.NewConnectionEx(context.Background(), server, M.Socksaddr{}, M.ParseSocksaddr("example.com:80"),
				func(err error) { closed <- err })
			flow, _ := pathPair(t)
			h.NewPacketConnectionEx(context.Background(), bufio.NewPacketConn(flow), M.Socksaddr{},
				M.ParseSocksaddr("example.com:53"), func(err error) { closed <- err })
			for range 2 {
				select {
				case err := <-closed:
					assert.Error(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("a stream or flow was neither routed nor refused")
				}
			}
			assert.Zero(t, r.count(), "nothing reaches the router")
		})
	}
}

// A later router replaces the earlier one's expiry.
func TestExitExpiryIsTheCurrentRouters(t *testing.T) {
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(100*time.Millisecond), alwaysReady)
	e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(time.Hour), alwaysReady)
	time.Sleep(300 * time.Millisecond)
	assert.True(t, e.Options().Ready(false))
	assert.NotNil(t, e.currentRouter(Pool))
}

func TestExitServeEndsAtOnceWhenNotServable(t *testing.T) {
	e := newTestExit(t)
	vConn, _ := pathPair(t)
	done := serve(t, e, vConn, "password")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve ran without an exit to serve")
	}
}

func TestExitRevocationDoesNotEndLaterSessions(t *testing.T) {
	r := newEchoRouter(t)
	e := readyExit(t, r)
	e.SetEnabled(false)
	e.SetEnabled(true)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	echo(t, testClient(t, uConn, pin, password))
}

func TestExitServeEndsWithItsContext(t *testing.T) {
	e := readyExit(t, newEchoRouter(t))
	vConn, _ := pathPair(t)
	_, password, _ := e.Options().Params(false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Options().Serve(ctx, vConn, password, false); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve outlived its context")
	}
}

func TestExitServeEndsWithItsPath(t *testing.T) {
	e := readyExit(t, newEchoRouter(t))
	vConn, _ := pathPair(t)
	_, password, _ := e.Options().Params(false)
	done := serve(t, e, vConn, password)
	time.Sleep(50 * time.Millisecond)
	_ = vConn.UDPConn.Close() // the ICE session ends: reads fail
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve outlived its path")
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's writes and the test's reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(t *testing.T, w io.Writer) sblog.ContextLogger {
	t.Helper()
	factory := sblog.NewDefaultFactory(context.Background(), sblog.Formatter{}, w, "", nil, false)
	factory.SetLevel(sblog.LevelTrace)
	require.NoError(t, factory.Start())
	t.Cleanup(func() { _ = factory.Close() })
	return factory.Logger()
}

func meteredExit(t *testing.T, lg sblog.ContextLogger) (*Exit, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	e, err := New(context.Background(), lg)
	require.NoError(t, err)
	t.Cleanup(e.Close)
	e.metrics.provider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	return e, reader
}

// counterSum returns the sum of name's points whose attributes include attrs.
func counterSum(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, name)
		points:
			for _, dp := range sum.DataPoints {
				for _, kv := range attrs {
					if v, ok := dp.Attributes.Value(kv.Key); !ok || v != kv.Value {
						continue points
					}
				}
				total += dp.Value
			}
		}
	}
	return total
}

func TestExitCountsRefusals(t *testing.T) {
	logs := &syncBuffer{}
	e, reader := meteredExit(t, debugLogger(t, logs))
	r := &refuseRouter{}
	e.SetEnabled(true)
	e.SetRouter(Pool, r, time.Now().Add(time.Hour), alwaysReady)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	c := testClient(t, uConn, pin, password)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialConn(ctx, M.ParseSocksaddr("secret.example.com:443"))
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		// The client sends its request with the first write.
		_, _ = conn.Write([]byte("GET"))
		_, err = conn.Read(make([]byte, 1))
		_ = conn.Close()
	}
	assert.Error(t, err, "the refused stream fails")
	require.Eventually(t, func() bool { return counterSum(t, reader, "unbounded.exit.refused") == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Contains(t, logs.String(), "rejected", "the failure's class is logged")
	assert.NotContains(t, logs.String(), "secret")
	assert.NotContains(t, logs.String(), "192.0.2.1")
	assert.NotContains(t, logs.String(), "127.0.0.1")
}

func TestExitCountsSessionsAndBytes(t *testing.T) {
	e, reader := meteredExit(t, sblog.NewNOPFactory().Logger())
	e.SetEnabled(true)
	e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(time.Hour), alwaysReady)
	e.Options().OnSession(true)
	e.Options().OnSession(true)
	e.Options().OnSession(false)
	assert.EqualValues(t, 2, counterSum(t, reader, "unbounded.exit.sessions", attribute.String("path", "ice")))
	assert.EqualValues(t, 1, counterSum(t, reader, "unbounded.exit.sessions", attribute.String("path", "webrtc")))

	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	echo(t, testClient(t, uConn, pin, password))
	require.Eventually(t, func() bool {
		return counterSum(t, reader, "unbounded.exit.io", attribute.String("direction", "up")) == 4 &&
			counterSum(t, reader, "unbounded.exit.io", attribute.String("direction", "down")) == 4
	}, 5*time.Second, 10*time.Millisecond)
}

func TestExitForcesBBR(t *testing.T) {
	o := serviceOptions(context.Background(), nil, nil)
	assert.True(t, o.IgnoreClientBandwidth)
	assert.Zero(t, o.ReceiveBPS)
	assert.Zero(t, o.SendBPS)
	assert.Equal(t, 2*time.Minute, o.UDPTimeout)
	assert.Nil(t, o.MasqueradeHandler)
	assert.Nil(t, o.RealmOptions)
	assert.Empty(t, o.SalamanderPassword)

	// A client that asks for Brutal still connects, on BBR.
	e := readyExit(t, newEchoRouter(t))
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	echo(t, testClient(t, uConn, pin, password, clientOpts{sendBPS: 10_000_000, receiveBPS: 10_000_000}))
}

func TestExitNeverLogsAUoTDestination(t *testing.T) {
	logs := &syncBuffer{}
	e, _ := meteredExit(t, debugLogger(t, logs))
	r := &dialRouter{}
	e.SetEnabled(true)
	e.SetRouter(Pool, r, time.Now().Add(time.Hour), alwaysReady)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	c := testClient(t, uConn, pin, password)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialConn(ctx, M.Socksaddr{Fqdn: uot.MagicAddress})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, uot.WriteRequest(conn, uot.Request{Destination: M.ParseSocksaddr("secret.example.com:53")}))
	require.Eventually(t, func() bool { return r.count() == 1 }, 5*time.Second, 10*time.Millisecond)
	md := r.lastRouted()
	assert.Equal(t, "secret.example.com:53", md.Destination.String(), "routed by the real destination")
	assert.Equal(t, InboundTag, md.Inbound)
	assert.NotContains(t, logs.String(), "secret")
}

// A client's QUIC close reason is its own text, and could carry anything: sing-quic logs the
// error it closes the session with, so the exit's logs must not see it.
func TestExitNeverLogsAClientsCloseReason(t *testing.T) {
	logs := &syncBuffer{}
	e, _ := meteredExit(t, debugLogger(t, logs))
	e.SetEnabled(true)
	e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(time.Hour), alwaysReady)
	vConn, uConn := pathPair(t)
	_, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)

	// A hand-made Hysteria2 client, as the test client always closes with an empty reason.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Only the PacketConn methods: quic-go would write to a *net.UDPConn's address.
	qc, err := quic.Dial(ctx, struct{ net.PacketConn }{uConn}, placeholderAddr, &stdtls.Config{InsecureSkipVerify: true, NextProtos: []string{"h3"}},
		&quic.Config{EnableDatagrams: true})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://hysteria/auth", nil)
	require.NoError(t, err)
	req.Header.Set("Hysteria-Auth", password)
	req.Header.Set("Hysteria-CC-RX", "0")
	resp, err := (&http3.Transport{}).NewClientConn(qc).RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 233, resp.StatusCode, "authenticated")
	require.NoError(t, qc.CloseWithError(0x101, "secret-close-reason"))

	time.Sleep(200 * time.Millisecond)
	assert.NotContains(t, logs.String(), "secret-close-reason")
}

// Each kind is ready on its own: enabled, with a router of its own whose registration hasn't
// run out.
func TestExitReadinessIsPerKind(t *testing.T) {
	e := newTestExit(t)
	o := e.Options()
	e.SetEnabled(true)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	assert.True(t, o.Ready(false))
	assert.False(t, o.Ready(true), "no personal router")
	_, _, ok := o.Params(true)
	assert.False(t, ok)
	e.SetRouter(Personal, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	assert.True(t, o.Ready(true))
	_, _, ok = o.Params(true)
	assert.True(t, ok)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(-time.Second), alwaysReady)
	assert.False(t, o.Ready(false), "pool registration expired")
	assert.True(t, o.Ready(true), "the personal one hasn't")
	e.SetEnabled(false)
	assert.False(t, o.Ready(true), "the flag gates both kinds")
}

// A personal session routes only through the personal router, and a pool session only through
// the pool router, with no fall back to the other kind's when its own is missing.
func TestExitRoutesEachKindThroughItsOwnRouter(t *testing.T) {
	pool, personal := newEchoRouter(t), newEchoRouter(t)
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, pool, time.Now().Add(time.Hour), alwaysReady)
	e.SetRouter(Personal, personal, time.Now().Add(time.Hour), alwaysReady)

	for _, kind := range []Kind{Personal, Pool} {
		vConn, uConn := pathPair(t)
		pin, password, ok := e.Options().Params(kind == Personal)
		require.True(t, ok)
		serveKind(t, e, vConn, password, kind)
		echo(t, testClient(t, uConn, pin, password))
	}
	assert.Equal(t, 1, personal.count(), "the personal session went through the personal router")
	assert.Equal(t, 1, pool.count(), "the pool session through the pool router")

	// With only one kind's router, the other kind's streams are refused, not crossed over.
	for _, tc := range []struct {
		cleared, other Kind
		otherRouter    *dialRouter
	}{
		{Personal, Pool, pool},
		{Pool, Personal, personal},
	} {
		e.SetRouter(Pool, pool, time.Now().Add(time.Hour), alwaysReady)
		e.SetRouter(Personal, personal, time.Now().Add(time.Hour), alwaysReady)
		e.SetRouter(tc.cleared, nil, time.Time{}, alwaysReady)
		before := tc.otherRouter.count()
		assert.Nil(t, e.currentRouter(tc.cleared))
		server, client := net.Pipe()
		closed := make(chan error, 1)
		(&handler{exit: e, kind: tc.cleared}).NewConnectionEx(context.Background(), server, M.Socksaddr{},
			M.ParseSocksaddr("example.com:80"), func(err error) { closed <- err })
		select {
		case err := <-closed:
			assert.Error(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the stream was neither routed nor refused")
		}
		_ = client.Close()
		assert.Equal(t, before, tc.otherRouter.count(), "a %s stream never reaches the %s router", tc.cleared, tc.other)
	}
}

// Clearing or expiring one kind's router ends only that kind's sessions.
func TestExitRevokingOneKindLeavesTheOther(t *testing.T) {
	for name, revoke := range map[string]func(*Exit){
		"cleared": func(e *Exit) { e.SetRouter(Personal, nil, time.Time{}, alwaysReady) },
		"expired": func(e *Exit) {
			e.SetRouter(Personal, newEchoRouter(t), time.Now().Add(100*time.Millisecond), alwaysReady)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestExit(t)
			e.SetEnabled(true)
			e.SetRouter(Pool, newEchoRouter(t), time.Now().Add(time.Hour), alwaysReady)
			e.SetRouter(Personal, newEchoRouter(t), time.Now().Add(time.Hour), alwaysReady)
			pConn, puConn := pathPair(t)
			pin, poolPassword, _ := e.Options().Params(false)
			poolDone := serveKind(t, e, pConn, poolPassword, Pool)
			fConn, _ := pathPair(t)
			_, personalPassword, _ := e.Options().Params(true)
			personalDone := serveKind(t, e, fConn, personalPassword, Personal)
			revoke(e)
			select {
			case <-personalDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the personal session outlived its router")
			}
			select {
			case <-poolDone:
				t.Fatal("the pool session ended with the personal router")
			case <-time.After(200 * time.Millisecond):
			}
			echo(t, testClient(t, puConn, pin, poolPassword))
			assert.True(t, e.Options().Ready(false))
			assert.False(t, e.Options().Ready(true))
		})
	}
}

// A kind out of range is ignored rather than panicking.
func TestExitIgnoresAnUnknownKind(t *testing.T) {
	e := newTestExit(t)
	e.SetEnabled(true)
	assert.NotPanics(t, func() { e.SetRouter(Kind(7), &dialRouter{}, time.Now().Add(time.Hour), alwaysReady) })
	assert.False(t, e.Options().Ready(false))
	assert.False(t, e.Options().Ready(true))
}

// The instruments follow the meter provider the embedder installs, including one it installs
// after the first measurement. The test stands in for the global provider through the metrics'
// lookup, leaving OpenTelemetry's global alone.
func TestExitMetricsFollowAProviderSwap(t *testing.T) {
	first, second := sdkmetric.NewManualReader(), sdkmetric.NewManualReader()
	var current metric.MeterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(first))
	m := newExitMetrics()
	m.lookup = func() metric.MeterProvider { return current }
	m.session(true)
	assert.EqualValues(t, 1, counterSum(t, first, "unbounded.exit.sessions"))
	current = sdkmetric.NewMeterProvider(sdkmetric.WithReader(second))
	m.session(true)
	m.refused()
	m.bytes(5, ioUp)
	assert.EqualValues(t, 1, counterSum(t, second, "unbounded.exit.sessions"), "the session after the swap reaches the new provider")
	assert.EqualValues(t, 1, counterSum(t, second, "unbounded.exit.refused"))
	assert.EqualValues(t, 5, counterSum(t, second, "unbounded.exit.io"))
	assert.EqualValues(t, 1, counterSum(t, first, "unbounded.exit.sessions"))
}

// sliceProvider is a provider whose type is comparable but whose value isn't: its interface
// field holds a slice, so == on two of them panics.
type sliceProvider struct {
	noop.MeterProvider
	v any
}

func TestExitMetricsTakeAnIncomparableProvider(t *testing.T) {
	m := newExitMetrics()
	m.lookup = func() metric.MeterProvider { return sliceProvider{v: []int{1}} }
	assert.NotPanics(t, func() {
		m.session(true)
		m.session(true)
		m.bytes(1, ioUp)
	})
}

// A rule set callback can still be running after its owner unregistered it and closed the exit,
// so SetRouter after Close installs nothing, not even an expiry timer.
func TestExitSetRouterAfterCloseIsANoOp(t *testing.T) {
	e := newTestExit(t)
	e.SetEnabled(true)
	e.Close()
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), alwaysReady)
	e.mu.Lock()
	ks := e.kinds[Pool]
	e.mu.Unlock()
	assert.Nil(t, ks.router)
	assert.Nil(t, ks.expiry)
	assert.False(t, e.Options().Ready(false))
}

// The exit checks its router's readiness, whether the box's abuse rule sets hold rules, at
// each stream and session, so a list that empties refuses new streams of running sessions
// until it holds rules again.
func TestExitChecksReadinessAtEachStream(t *testing.T) {
	var empty atomic.Bool
	ready := func() error {
		if empty.Load() {
			return errors.New("emptied")
		}
		return nil
	}
	r := newEchoRouter(t)
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, r, time.Now().Add(time.Hour), ready)
	vConn, uConn := pathPair(t)
	pin, password, ok := e.Options().Params(false)
	require.True(t, ok)
	serve(t, e, vConn, password)
	c := testClient(t, uConn, pin, password)
	echo(t, c)

	empty.Store(true)
	assert.False(t, e.Options().Ready(false))
	_, _, ok = e.Options().Params(false)
	assert.False(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialConn(ctx, M.ParseSocksaddr("example.com:80"))
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write([]byte("ping"))
		_, err = conn.Read(make([]byte, 4))
		_ = conn.Close()
	}
	assert.Error(t, err, "a new stream is refused while the list is empty")
	assert.Equal(t, 1, r.count(), "nothing more reaches the router")

	empty.Store(false)
	assert.True(t, e.Options().Ready(false))
	echo(t, c)
	assert.Equal(t, 2, r.count(), "the same session's streams route again once it holds rules")
}

// A router without a ready check is never ready.
func TestExitNeedsAReadyCheck(t *testing.T) {
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, &dialRouter{}, time.Now().Add(time.Hour), nil)
	assert.False(t, e.Options().Ready(false))
	assert.Nil(t, e.currentRouter(Pool))
}

// A UoT stream's header is the client's to send whenever it likes, and sing-box's UoT router
// waits for it before routing. The exit checks readiness again, and takes the kind's router
// then current, once the header has arrived, so a header held back while a set empties is
// refused.
func TestExitChecksReadinessAfterTheUoTHeader(t *testing.T) {
	var empty atomic.Bool
	ready := func() error {
		if empty.Load() {
			return errors.New("emptied")
		}
		return nil
	}
	r := &dialRouter{}
	e := newTestExit(t)
	e.SetEnabled(true)
	e.SetRouter(Pool, r, time.Now().Add(time.Hour), ready)
	vConn, uConn := pathPair(t)
	pin, password, _ := e.Options().Params(false)
	serve(t, e, vConn, password)
	c := testClient(t, uConn, pin, password)

	var header bytes.Buffer
	require.NoError(t, uot.WriteRequest(&header, uot.Request{Destination: M.ParseSocksaddr("example.com:53")}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := c.DialConn(ctx, M.Socksaddr{Fqdn: uot.MagicAddress})
	require.NoError(t, err)
	defer conn.Close()
	// The stream's request goes with the first byte; the rest of the header waits.
	_, err = conn.Write(header.Bytes()[:1])
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)
	empty.Store(true)
	_, err = conn.Write(header.Bytes()[1:])
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, r.count(), "a UoT flow whose header came after a set emptied reached the router")

	empty.Store(false)
	conn2, err := c.DialConn(ctx, M.Socksaddr{Fqdn: uot.MagicAddress})
	require.NoError(t, err)
	defer conn2.Close()
	_, err = conn2.Write(header.Bytes())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return r.count() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a new UoT flow routes once the set holds rules")
}
