//go:build with_quic

package unbounded

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	UBClientcore "github.com/getlantern/broflake/clientcore"
	"github.com/sagernet/sing-box/common/tls"
	sblog "github.com/sagernet/sing-box/log"
	sbOption "github.com/sagernet/sing-box/option"
	qtls "github.com/sagernet/sing-quic"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/getlantern/lantern-box/protocol/internal/quicjunk"
)

const (
	// exitFailureLimit path failures in a row turn direct exits off for exitBackoff.
	exitFailureLimit = 3
	exitBackoff      = 10 * time.Minute
)

var (
	errPathDead   = errors.New("unbounded: direct exit path is gone")
	errPathClosed = errors.New("unbounded: direct exit path closed")
)

// exitPathAddr is the address a direct exit path's conn writes to. The conn has one peer, the
// volunteer, and ignores it.
var exitPathAddr = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}

// exitPaths holds the direct exit paths broflake hands the outbound, each with its own
// Hysteria2 client to the volunteer's exit, and dials through the newest live one.
//
// Nothing about a path is ever logged: its pin, password and conn are the session's
// credentials, and the client's errors can carry the pin.
type exitPaths struct {
	ctx        context.Context
	cancel     context.CancelFunc
	logger     logger.ContextLogger
	serverName string
	// idleTimeout is the clients' QUIC idle timeout; zero means sing-quic's default.
	idleTimeout time.Duration
	now         func() time.Time
	metrics     *exitPathMetrics

	mu    sync.Mutex
	paths []*exitPath // oldest first
	// failures counts path failures since the last success or back-off.
	failures     int
	backoffUntil time.Time
	closed       bool
}

type exitPath struct {
	client *hysteria2.Client
	conn   *packetConnAsConn
	// succeeded is set once the path has carried a dial, which proves its QUIC handshake, pin
	// and password. After that, an error is the destination's, not the path's.
	succeeded atomic.Bool
	closeOnce sync.Once
}

// dead reports whether the path's conn is closed: its QUIC connection failed or ended, or its
// client tried to dial a second one.
func (p *exitPath) dead() bool { return p.conn.closed.Load() }

func (p *exitPath) close() {
	p.closeOnce.Do(func() {
		if p.client != nil {
			_ = p.client.CloseWithError(errPathClosed)
		}
		_ = p.conn.Close()
	})
}

func newExitPaths(ctx context.Context, lg logger.ContextLogger, serverName string) *exitPaths {
	ctx, cancel := context.WithCancel(ctx)
	return &exitPaths{
		ctx:        ctx,
		cancel:     cancel,
		logger:     lg,
		serverName: serverName,
		now:        time.Now,
		metrics:    &exitPathMetrics{},
	}
}

// onExitPath is broflake's WebRTCOptions.OnExitPath. It runs on the consumer worker, so it only
// builds the path's client; nothing connects until a dial.
func (p *exitPaths) onExitPath(ep UBClientcore.ExitPath) {
	conn := &packetConnAsConn{PacketConn: ep.Conn}
	path := &exitPath{conn: conn}
	pin, err := base64.StdEncoding.DecodeString(ep.CertPin)
	if err != nil || len(pin) != sha256.Size {
		path.close()
		p.fail()
		p.logger.DebugContext(p.ctx, "unbounded direct exit path dropped: bad certificate pin")
		return
	}
	// sing-box's TLS and sing-quic's client get no logger: what they log about a failed
	// handshake can include the pin.
	nop := sblog.NewNOPFactory().Logger()
	tlsConfig, err := tls.NewClient(p.ctx, nop, p.serverName, sbOption.OutboundTLSOptions{
		Enabled:                    true,
		ServerName:                 p.serverName,
		ALPN:                       badoption.Listable[string]{"h3"},
		CertificatePublicKeySHA256: badoption.Listable[[]byte]{pin},
	})
	if err != nil {
		path.close()
		p.logger.DebugContext(p.ctx, "unbounded direct exit path dropped: TLS config")
		return
	}
	client, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       p.ctx,
		Dialer:        &pathDialer{conn: conn},
		Logger:        nop,
		ServerAddress: M.ParseSocksaddrHostPort(p.serverName, 443),
		// BBR: a zero send rate never negotiates Brutal.
		SendBPS:     0,
		ReceiveBPS:  0,
		Password:    ep.Password,
		TLSConfig:   tlsConfig,
		QUICOptions: qtls.QUICOptions{IdleTimeout: p.idleTimeout},
	})
	if err != nil {
		path.close()
		p.logger.DebugContext(p.ctx, "unbounded direct exit path dropped: client")
		return
	}
	path.client = client

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		path.close()
		return
	}
	p.paths = append(p.paths, path)
	p.mu.Unlock()
	p.metrics.path(ep.Relayed)
}

// takeExit is broflake's WebRTCOptions.TakeExit: false while backing off after repeated path
// failures, and once closed.
func (p *exitPaths) takeExit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && !p.now().Before(p.backoffUntil)
}

// dial dials destination through the newest live path. It returns errNoExitPath when there is
// none.
func (p *exitPaths) dial(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch N.NetworkName(network) {
	case N.NetworkTCP:
		return usePath(p, ctx, func(c *hysteria2.Client) (net.Conn, error) {
			return c.DialConn(ctx, destination)
		})
	case N.NetworkUDP:
		return usePath(p, ctx, func(c *hysteria2.Client) (net.Conn, error) {
			pc, err := c.ListenPacket(ctx)
			if err != nil {
				return nil, err
			}
			return bufio.NewBindPacketConn(pc, destination), nil
		})
	}
	return nil, errors.New("unbounded: unsupported network for a direct exit")
}

// listenPacket opens a UDP session through the newest live path, or returns errNoExitPath.
func (p *exitPaths) listenPacket(ctx context.Context) (net.PacketConn, error) {
	return usePath(p, ctx, func(c *hysteria2.Client) (net.PacketConn, error) {
		return c.ListenPacket(ctx)
	})
}

// usePath runs f on the newest live path's client. sing-quic's DialConn and ListenPacket finish
// the QUIC handshake and Hysteria2 authentication before they return (only a stream's request
// waits for its first write), so f succeeding proves the path. An error on a path that never
// succeeded is the path's: it counts as a failure, and the next path is tried. An error on a
// path that has succeeded is the destination's, and is returned, unless the path has since
// died.
func usePath[T any](p *exitPaths, ctx context.Context, f func(*hysteria2.Client) (T, error)) (T, error) {
	var zero T
	for {
		path := p.newest()
		if path == nil {
			return zero, errNoExitPath
		}
		v, err := f(path.client)
		if err == nil {
			p.succeed(path)
			return v, nil
		}
		if ctx.Err() != nil {
			// The caller gave up, which says nothing about the path. Its error, not the
			// client's, which can carry the pin.
			if path.dead() {
				p.drop(path)
			}
			return zero, ctx.Err()
		}
		switch {
		case !path.succeeded.Load():
			p.drop(path)
			p.fail()
			p.logger.DebugContext(ctx, "unbounded direct exit path failed")
		case path.dead():
			p.drop(path)
		default:
			return zero, err
		}
	}
}

// newest returns the newest live path, dropping dead ones, or nil.
func (p *exitPaths) newest() *exitPath {
	var dead []*exitPath
	defer func() {
		for _, path := range dead {
			path.close()
		}
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.paths) > 0 {
		path := p.paths[len(p.paths)-1]
		if !path.dead() {
			return path
		}
		dead = append(dead, path)
		p.paths[len(p.paths)-1] = nil
		p.paths = p.paths[:len(p.paths)-1]
	}
	return nil
}

func (p *exitPaths) drop(path *exitPath) {
	p.mu.Lock()
	for i, q := range p.paths {
		if q == path {
			p.paths = append(p.paths[:i], p.paths[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	path.close()
}

func (p *exitPaths) succeed(path *exitPath) {
	path.succeeded.Store(true)
	p.mu.Lock()
	p.failures = 0
	p.mu.Unlock()
}

func (p *exitPaths) fail() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures++
	if p.failures >= exitFailureLimit {
		p.failures = 0
		p.backoffUntil = p.now().Add(exitBackoff)
	}
}

func (p *exitPaths) failureCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures
}

// close closes every path's client and conn. Paths that arrive later are closed at once.
func (p *exitPaths) close() {
	p.mu.Lock()
	p.closed = true
	paths := p.paths
	p.paths = nil
	p.mu.Unlock()
	p.cancel()
	for _, path := range paths {
		path.close()
	}
}

// pathDialer hands a path's client its conn. sing-quic dials once per QUIC connection, so a
// second dial means the first connection died: the path is done, and the dial fails.
type pathDialer struct {
	conn *packetConnAsConn
	used atomic.Bool
}

func (d *pathDialer) DialContext(_ context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkUDP {
		return nil, errors.New("unbounded: a direct exit path carries only UDP")
	}
	if d.used.Swap(true) {
		_ = d.conn.Close()
		return nil, errPathDead
	}
	// The junk goes first, so the QUIC Initial is never the path's first datagram.
	junk, err := quicjunk.New()
	if err == nil {
		_, err = d.conn.Write(junk)
	}
	if err != nil {
		_ = d.conn.Close()
		return nil, errPathDead
	}
	return d.conn, nil
}

func (d *pathDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("unbounded: a direct exit path carries only UDP")
}

// packetConnAsConn is a net.Conn over a packet conn with one peer, as a direct exit path's is.
// Closing it closes the path's conn, which ends the ICE session and frees broflake's slot.
type packetConnAsConn struct {
	net.PacketConn
	closed atomic.Bool
}

func (c *packetConnAsConn) Read(b []byte) (int, error) {
	n, _, err := c.PacketConn.ReadFrom(b)
	return n, err
}

func (c *packetConnAsConn) Write(b []byte) (int, error) {
	return c.PacketConn.WriteTo(b, exitPathAddr)
}

func (c *packetConnAsConn) RemoteAddr() net.Addr { return exitPathAddr }

func (c *packetConnAsConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.PacketConn.Close()
}

// exitPathMetrics counts the direct exit paths the outbound takes. Never an address.
type exitPathMetrics struct {
	// provider is the meter provider; nil means the global one, looked up on first use.
	provider metric.MeterProvider

	once  sync.Once
	paths metric.Int64Counter
}

var (
	exitPathDirect  = metric.WithAttributeSet(attribute.NewSet(attribute.String("kind", "ice"), attribute.Bool("relayed", false)))
	exitPathRelayed = metric.WithAttributeSet(attribute.NewSet(attribute.String("kind", "ice"), attribute.Bool("relayed", true)))
)

func (m *exitPathMetrics) path(relayed bool) {
	m.once.Do(func() {
		pr := m.provider
		if pr == nil {
			pr = otel.GetMeterProvider()
		}
		var err error
		if m.paths, err = pr.Meter("lantern-box/unbounded").Int64Counter("unbounded.path",
			metric.WithDescription("Paths the Unbounded outbound took, by kind and whether ICE chose a TURN relay")); err != nil {
			m.paths, _ = noop.Meter{}.Int64Counter("")
		}
	})
	if relayed {
		m.paths.Add(context.Background(), 1, exitPathRelayed)
	} else {
		m.paths.Add(context.Background(), 1, exitPathDirect)
	}
}
