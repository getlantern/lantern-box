package meek

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/uot"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/protocol/socks/socks5"

	"github.com/getlantern/lantern-box/constant"
	L "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/option"
)

// RegisterInbound registers the meek inbound adapter with the given registry.
func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.MeekInboundOptions](registry, constant.TypeMeek, NewInbound)
}

// Inbound is the sing-box inbound adapter for a meek server. It serves the
// meek-v1 HTTP polling protocol (plain HTTP — a CDN/Caddy terminates TLS and
// fronting in front) and routes each tunneled session through sing-box.
//
// The bundled meek outbound opens every session with a SOCKS5 CONNECT, so the
// inbound runs a tiny in-process SOCKS5 acceptor on loopback as the meek
// server's upstream: it terminates the CONNECT, extracts the destination, and
// hands the post-CONNECT stream to the router. This replaces the external SOCKS5
// proxy (microsocks) that the standalone cmd/meek-server relies on, so the data
// plane is native sing-box (routing rules, the configured outbound, metrics all
// apply).
type Inbound struct {
	inbound.Adapter
	ctx         context.Context
	logger      log.ContextLogger
	router      adapter.ConnectionRouterEx
	listener    *listener.Listener
	tcpListener net.Listener
	httpServer  *http.Server
	meekServer  *Server
	socksLn     net.Listener
	closeOnce   sync.Once
}

// NewInbound constructs a meek inbound adapter.
func NewInbound(
	ctx context.Context,
	router adapter.Router,
	logger log.ContextLogger,
	tag string,
	options option.MeekInboundOptions,
) (adapter.Inbound, error) {
	// A meek inbound is a public/fronted relay into sing-box, so an empty auth
	// token would make it an open relay. Require one unless the operator explicitly
	// opts into the no-auth mode (test/private deployments only).
	if options.AuthToken == "" && !options.AllowUnauthenticated {
		return nil, errors.New(
			"meek: auth_token is required (a meek inbound is a public relay into sing-box); " +
				"set auth_token, or set allow_unauthenticated:true to deliberately run an open relay",
		)
	}
	responseHoldoff, err := parseDurationOr(options.ResponseHoldoff, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("meek: response_holdoff: %w", err)
	}
	sessionIdleTimeout, err := parseDurationOr(options.SessionIdleTimeout, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("meek: session_idle_timeout: %w", err)
	}
	readTimeout, err := parseDurationOr(options.ReadTimeout, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("meek: read_timeout: %w", err)
	}
	writeTimeout, err := parseDurationOr(options.WriteTimeout, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("meek: write_timeout: %w", err)
	}
	idleTimeout, err := parseDurationOr(options.IdleTimeout, 120*time.Second)
	if err != nil {
		return nil, fmt.Errorf("meek: idle_timeout: %w", err)
	}

	// In-process SOCKS5 acceptor on loopback. The meek server dials this per
	// session (replacing microsocks); handleSocks terminates the CONNECT and
	// routes through sing-box. Loopback-only, so it isn't externally reachable.
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("meek: loopback socks listener: %w", err)
	}

	ib := &Inbound{
		Adapter: inbound.NewAdapter(constant.TypeMeek, tag),
		ctx:     ctx,
		logger:  logger,
		router:  uot.NewRouter(router, logger),
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
		socksLn: socksLn,
	}

	tcpListener, err := ib.listener.ListenTCP()
	if err != nil {
		_ = socksLn.Close()
		return nil, fmt.Errorf("meek: tcp listener: %w", err)
	}
	ib.tcpListener = tcpListener

	meekServer, err := NewServer(ServerConfig{
		Upstream:           socksLn.Addr().String(),
		MaxBodyBytes:       options.MaxBodyBytes,
		ResponseHoldoff:    responseHoldoff,
		SessionIdleTimeout: sessionIdleTimeout,
		AuthToken:          options.AuthToken,
		// Route meek-server logs through the inbound's sing-box logger (matches
		// WATER), not slog.Default(), so levels/routing stay consistent.
		Logger: slog.New(L.NewLogHandler(logger)),
	})
	if err != nil {
		_ = socksLn.Close()
		_ = tcpListener.Close()
		return nil, fmt.Errorf("meek: server: %w", err)
	}
	ib.meekServer = meekServer
	ib.httpServer = &http.Server{
		Handler:      meekServer,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}
	return ib, nil
}

// Start serves the meek HTTP endpoint and the loopback SOCKS5 acceptor.
func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	go i.acceptSocks()
	go func() {
		if err := i.httpServer.Serve(i.tcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			i.logger.Error("meek inbound http server: ", err)
		}
	}()
	return nil
}

// acceptSocks accepts loopback connections from the meek server and routes each
// through sing-box after terminating its SOCKS5 CONNECT.
func (i *Inbound) acceptSocks() {
	var failures int
	for {
		conn, err := i.socksLn.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed on Close()
			}
			// A transient Accept error must not permanently kill routing while the
			// HTTP endpoint keeps serving — log and keep accepting, but back off
			// (capped at 1s) so a persistent error (e.g. FD exhaustion) can't spin
			// hot and flood the log.
			failures++
			backoff := min(time.Duration(failures)*10*time.Millisecond, time.Second)
			i.logger.Error("meek inbound: socks accept (retry in ", backoff, "): ", err)
			time.Sleep(backoff)
			continue
		}
		failures = 0
		go i.handleSocks(conn)
	}
}

// handleSocks terminates the SOCKS5 no-auth CONNECT the meek outbound sends to
// open a session, then routes the tunneled stream through sing-box.
func (i *Inbound) handleSocks(conn net.Conn) {
	// Read through a bufio.Reader: it satisfies socks5's varbin.Reader (Read +
	// ReadByte) and preserves any bytes buffered past the request for routing.
	br := bufio.NewReader(conn)

	authReq, err := socks5.ReadAuthRequest(br)
	if err != nil {
		i.logger.Debug("meek inbound: read socks auth: ", err)
		_ = conn.Close()
		return
	}
	if !containsByte(authReq.Methods, socks5.AuthTypeNotRequired) {
		_ = socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: 0xFF}) // no acceptable methods
		_ = conn.Close()
		return
	}
	if err := socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: socks5.AuthTypeNotRequired}); err != nil {
		_ = conn.Close()
		return
	}

	req, err := socks5.ReadRequest(br)
	if err != nil {
		i.logger.Debug("meek inbound: read socks request: ", err)
		_ = conn.Close()
		return
	}
	if req.Command != socks5.CommandConnect {
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeUnsupported})
		_ = conn.Close()
		return
	}
	if err := socks5.WriteResponse(conn, socks5.Response{
		ReplyCode: socks5.ReplyCodeSuccess,
		Bind:      M.SocksaddrFromNet(conn.LocalAddr()),
	}); err != nil {
		_ = conn.Close()
		return
	}

	routed := &bufferedConn{Conn: conn, r: br}
	var metadata adapter.InboundContext
	metadata.Inbound = i.Tag()
	metadata.InboundType = i.Type()
	metadata.Source = M.SocksaddrFromNet(conn.RemoteAddr()).Unwrap()
	metadata.Destination = req.Destination
	i.router.RouteConnectionEx(i.ctx, routed, metadata, func(err error) {
		if err != nil {
			i.logger.Debug("meek inbound: route ", req.Destination, ": ", err)
		}
	})
}

// Close stops the HTTP server, the meek server (and its sessions), the loopback
// acceptor, and the sing-box listener.
func (i *Inbound) Close() error {
	var httpErr, meekErr, socksErr, lnErr error
	i.closeOnce.Do(func() {
		if i.httpServer != nil {
			httpErr = i.httpServer.Close()
		}
		if i.meekServer != nil {
			meekErr = i.meekServer.Close()
		}
		if i.socksLn != nil {
			socksErr = i.socksLn.Close()
		}
		if i.listener != nil {
			lnErr = i.listener.Close()
		}
	})
	return errors.Join(httpErr, meekErr, socksErr, lnErr)
}

// bufferedConn reads through a bufio.Reader (so bytes buffered past the SOCKS5
// request aren't lost) while writes/close/addrs/deadlines pass to the conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
