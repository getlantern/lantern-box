//go:build with_quic

// Package hysteria2x is the stock sing-box hysteria2 outbound with Lantern's
// client-side QUIC-censorship evasions layered on top. It talks to an unmodified
// hysteria2 inbound. Evasions hook in through the dialer handed to sing-quic, so
// sing-quic and sing-box stay unforked and pick up upstream fixes directly.
package hysteria2x

import (
	"context"
	"crypto/rand"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-quic/hysteria"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register(registry, constant.TypeHysteria2X, NewOutbound)
}

type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	client *hysteria2.Client
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.Hysteria2XOutboundOptions) (adapter.Outbound, error) {
	options.UDPFragmentDefault = true
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, C.ErrTLSRequired
	}
	tlsConfig, err := tls.NewClient(ctx, logger, options.Server, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, err
	}
	var salamanderPassword string
	if options.Obfs != nil {
		if options.Obfs.Password == "" {
			return nil, E.New("missing obfs password")
		}
		switch options.Obfs.Type {
		case hysteria2.ObfsTypeSalamander:
			salamanderPassword = options.Obfs.Password
		default:
			return nil, E.New("unknown obfs type: ", options.Obfs.Type)
		}
	}
	var outboundDialer N.Dialer
	outboundDialer, err = dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}
	if options.PreInitialJunk {
		outboundDialer = &junkDialer{Dialer: outboundDialer}
	}
	networkList := options.Network.Build()
	client, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:            ctx,
		Dialer:             outboundDialer,
		Logger:             logger,
		BrutalDebug:        options.BrutalDebug,
		ServerAddress:      options.ServerOptions.Build(),
		ServerPorts:        options.ServerPorts,
		HopInterval:        time.Duration(options.HopInterval),
		SendBPS:            uint64(options.UpMbps * hysteria.MbpsToBps),
		ReceiveBPS:         uint64(options.DownMbps * hysteria.MbpsToBps),
		SalamanderPassword: salamanderPassword,
		Password:           options.Password,
		TLSConfig:          tlsConfig,
		UDPDisabled:        !common.Contains(networkList, N.NetworkUDP),
	})
	if err != nil {
		return nil, err
	}
	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(constant.TypeHysteria2X, tag, networkList, options.DialerOptions),
		logger:  logger,
		client:  client,
	}, nil
}

func (h *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch N.NetworkName(network) {
	case N.NetworkTCP:
		h.logger.InfoContext(ctx, "outbound connection to ", destination)
		return h.client.DialConn(ctx, destination)
	case N.NetworkUDP:
		conn, err := h.ListenPacket(ctx, destination)
		if err != nil {
			return nil, err
		}
		return bufio.NewBindPacketConn(conn, destination), nil
	default:
		return nil, E.New("unsupported network: ", network)
	}
}

func (h *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	h.logger.InfoContext(ctx, "outbound packet connection to ", destination)
	return h.client.ListenPacket(ctx)
}

func (h *Outbound) InterfaceUpdated() {
	h.client.CloseWithError(E.New("network changed"))
}

func (h *Outbound) Close() error {
	return h.client.CloseWithError(os.ErrClosed)
}

// junkDialer writes one random datagram on every new UDP flow before handing
// the socket to QUIC, so the Initial is never the flow's first datagram.
// sing-quic dials a fresh socket per port hop, so each hop gets its own junk.
type junkDialer struct {
	N.Dialer
}

func (d *junkDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	conn, err := d.Dialer.DialContext(ctx, network, destination)
	if err != nil || N.NetworkName(network) != N.NetworkUDP {
		return conn, err
	}
	junk, err := newJunk()
	if err == nil {
		_, err = conn.Write(junk)
	}
	if err != nil {
		conn.Close()
		return nil, E.Cause(err, "write pre-initial junk")
	}
	return conn, nil
}

const (
	minJunkLen = 8
	maxJunkLen = 64
)

// newJunk returns 8–64 random bytes with the QUIC long-header bit set. Servers
// silently drop long-header packets shorter than 1200 bytes whose version they
// don't support, and so never answer the junk.
func newJunk() ([]byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxJunkLen-minJunkLen+1))
	if err != nil {
		return nil, err
	}
	junk := make([]byte, minJunkLen+int(n.Int64()))
	if _, err := rand.Read(junk); err != nil {
		return nil, err
	}
	junk[0] |= 0x80
	return junk, nil
}
