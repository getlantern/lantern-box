package algeneva

import (
	"bufio"
	"context"
	"fmt"
	"net"

	alg "github.com/getlantern/algeneva"
	"github.com/gobwas/ws"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.ALGenevaInboundOptions](registry, constant.TypeALGeneva, NewInbound)
}

// Inbound is a wrapper around [http.Inbound] that listens for connections using the Application
// Layer Geneva HTTP protocol.
type Inbound struct {
	inbound.Adapter
	httpInbound *http.Inbound
	listener    *listener.Listener
	logger      log.ContextLogger
}

// NewInbound creates a new algeneva inbound adapter.
func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.ALGenevaInboundOptions) (adapter.Inbound, error) {
	httpInbound, err := http.NewInbound(ctx, router, logger, tag, options.HTTPMixedInboundOptions)
	if err != nil {
		return nil, err
	}
	a := &Inbound{
		Adapter:     inbound.NewAdapter(constant.TypeALGeneva, tag),
		httpInbound: httpInbound.(*http.Inbound),
		logger:      logger,
	}
	a.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            logger,
		Network:           []string{N.NetworkTCP},
		Listen:            options.ListenOptions,
		ConnectionHandler: a,
	})
	return a, nil
}

func (a *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	return a.listener.Start()
}

func (a *Inbound) Close() error {
	return a.listener.Close()
}

func (a *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	metadata.Inbound = a.Tag()
	metadata.InboundType = a.Type()
	conn, err := a.newConnectionEx(ctx, conn)
	if err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, err)
		a.logger.ErrorContext(ctx, exceptions.Cause(err, "process connection from ", metadata.Source))
		return
	}
	a.httpInbound.NewConnectionEx(ctx, conn, metadata, onClose)
}

// newConnectionEx processes the connection and upgrades it to a WebSocket connection.
func (a *Inbound) newConnectionEx(ctx context.Context, conn net.Conn) (net.Conn, error) {
	a.logger.DebugContext(ctx, "processing connection")
	reader := bufio.NewReader(conn)
	request, err := alg.ReadRequest(reader)
	if err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	if request.Method != "CONNECT" {
		return nil, fmt.Errorf("unexpected method: %s", request.Method)
	}
	a.logger.TraceContext(ctx, "sending response")
	_, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if err != nil {
		return nil, fmt.Errorf("writing response: %w", err)
	}
	a.logger.TraceContext(ctx, "upgrading connection to ws")
	_, err = ws.Upgrade(conn)
	if err != nil {
		return nil, fmt.Errorf("websocket upgrade: %w", err)
	}
	return conn, nil
}
