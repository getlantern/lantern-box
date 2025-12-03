package algeneva

import (
	"bufio"
	"context"
	"fmt"
	"net"

	alg "github.com/getlantern/algeneva"
	"github.com/gobwas/ws"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/metrics"
	"github.com/getlantern/lantern-box/option"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/network"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.ALGenevaInboundOptions](registry, constant.TypeALGeneva, NewInbound)
}

// Inbound is a wrapper around [http.Inbound] that listens for connections using the Application
// Layer Geneva HTTP protocol.
type Inbound struct {
	http.Inbound
	logger log.ContextLogger
}

// NewInbound creates a new algeneva inbound adapter.
func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.ALGenevaInboundOptions) (adapter.Inbound, error) {
	httpInbound, err := http.NewInbound(ctx, router, logger, tag, options.HTTPMixedInboundOptions)
	if err != nil {
		return nil, err
	}
	inbound := httpInbound.(*http.Inbound)
	return &Inbound{
		Inbound: *inbound,
		logger:  logger,
	}, nil
}

func (a *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose network.CloseHandlerFunc) {
	metadata.Inbound = a.Tag()
	metadata.InboundType = a.Type()
	conn = metrics.NewConn(conn, &metadata)
	conn, err := a.newConnectionEx(ctx, conn)
	if err != nil {
		network.CloseOnHandshakeFailure(conn, onClose, err)
		a.logger.ErrorContext(ctx, exceptions.Cause(err, "process connection from ", metadata.Source))
		return
	}
	a.Inbound.NewConnectionEx(ctx, conn, metadata, onClose)
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
