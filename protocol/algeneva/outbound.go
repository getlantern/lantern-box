package algeneva

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gobwas/ws"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/tls"
	singConsts "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	sHTTP "github.com/sagernet/sing/protocol/http"

	alg "github.com/getlantern/algeneva"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.ALGenevaOutboundOptions](registry, constant.TypeALGeneva, NewOutbound)
}

// Outbound is a wrapper around [sHTTP.Client] that implements the Application Layer Geneva HTTP protocol.
type Outbound struct {
	outbound.Adapter
	strategy *alg.HTTPStrategy
	client   *sHTTP.Client
	logger   logger.ContextLogger
}

// NewOutbound creates a new Application Layer Geneva HTTP outbound adapter.
func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.ALGenevaOutboundOptions) (adapter.Outbound, error) {
	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}

	strategy, err := alg.NewHTTPStrategy(options.Strategy)
	if err != nil {
		logger.Error("parsing strategy: ", err)
		return nil, fmt.Errorf("parsing strategy: %v", err)
	}
	alDialer := &aDialer{
		Dialer:   outboundDialer,
		strategy: strategy,
		logger:   logger,
	}

	detour, err := tls.NewDialerFromOptions(ctx, router, alDialer, options.Server, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(singConsts.TypeHTTP, tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:  logger,
		client: sHTTP.NewClient(sHTTP.Options{
			Dialer:   detour,
			Server:   options.ServerOptions.Build(),
			Username: options.Username,
			Password: options.Password,
			Path:     options.Path,
			Headers:  options.Headers.Build(),
		}),
	}, nil
}

// DialContext dials a connection to the destination.
func (a *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = a.Tag()
	metadata.Destination = destination
	conn, err := a.client.DialContext(ctx, network, destination)
	if err != nil {
		a.logger.ErrorContext(ctx, "failed to connect to ", destination, " ", err)
	} else {
		a.logger.InfoContext(ctx, "connected to ", destination)
	}
	return conn, err
}

// ListenPacket is not supported.
func (a *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

// aDialer is a wrapper around [network.Dialer] that applies the ALGeneva strategy to a CONNECT request
// that is sent to the proxy server. Once the connection is established, the connection is upgraded to a
// WebSocket connection.
type aDialer struct {
	network.Dialer
	strategy *alg.HTTPStrategy
	logger   log.ContextLogger
}

func (d *aDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	d.logger.InfoContext(ctx, "dialing ", destination.String())
	conn, err := d.Dialer.DialContext(ctx, network, destination)
	if err != nil {
		return nil, err
	}

	if destination.Port == 0 {
		destination.Port = 80
	}
	uri, err := url.ParseRequestURI("http://" + destination.String())
	if err != nil {
		return nil, err
	}
	request := strings.Join([]string{
		"CONNECT " + uri.Host + " HTTP/1.1",
		"Host: " + destination.AddrString(),
		"Proxy-Connection: keep-alive",
	}, "\r\n")
	request += "\r\n\r\n"
	d.logger.TraceContext(ctx, "applying strategy")
	req, err := d.strategy.Apply([]byte(request))
	if err != nil {
		return nil, err
	}
	d.logger.TraceContext(ctx, "sending modified request")
	_, err = conn.Write(req)
	if err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err
	}
	d.logger.TraceContext(ctx, "response: ", resp.Status)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	d.logger.TraceContext(ctx, "upgrading connection to ws")
	_, _, err = ws.Dialer{}.Upgrade(conn, uri)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ListenPacket is not supported.
func (d *aDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}
