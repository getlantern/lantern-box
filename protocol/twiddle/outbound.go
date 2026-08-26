// Package twiddle adapts the twiddle transport to sing-box.
//
// The transport itself lives in github.com/getlantern/twiddle and carries no
// sing-box dependency; this package is the thin layer that registers it,
// parses options and wires the dialer, following the samizdat and algeneva
// pattern rather than reflex's fully in-tree one.
package twiddle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
	tw "github.com/getlantern/twiddle"
)

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.TwiddleOutboundOptions](registry, constant.TypeTwiddle, NewOutbound)
}

type Outbound struct {
	outbound.Adapter
	logger  logger.ContextLogger
	dialer  N.Dialer
	server  string
	port    uint16
	timeout time.Duration

	// mu guards cred, which rotates on every connection: the egress issues the
	// next credential inside each flight. sing-box dials concurrently, so
	// without this two dials could race and either lose a rotation or present
	// the same single-use ticket twice.
	mu   sync.Mutex
	cred *tw.Credential
	cfg  tw.ClientConfig
}

func NewOutbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.TwiddleOutboundOptions) (adapter.Outbound, error) {
	ticket, err := base64.StdEncoding.DecodeString(options.Ticket)
	if err != nil {
		return nil, fmt.Errorf("twiddle: bad ticket: %w", err)
	}
	psk, err := hex.DecodeString(options.PSK)
	if err != nil {
		return nil, fmt.Errorf("twiddle: bad psk: %w", err)
	}
	cred, err := tw.CredentialFromWire(ticket, psk)
	if err != nil {
		return nil, err
	}

	pool := tw.DefaultPool()
	if options.HelloPool != "" {
		if pool, err = tw.ParsePool(options.HelloPool); err != nil {
			return nil, err
		}
	}
	if options.CoverSNI == "" {
		return nil, fmt.Errorf("twiddle: cover_sni is required; it must agree with the egress's masquerade_upstream")
	}

	timeout := 15 * time.Second
	if options.ConnectTimeout != "" {
		if timeout, err = time.ParseDuration(options.ConnectTimeout); err != nil {
			return nil, fmt.Errorf("twiddle: invalid connect_timeout: %w", err)
		}
	}
	outboundDialer, err := dialer.New(ctx, options.DialerOptions, options.ServerIsDomain())
	if err != nil {
		return nil, err
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions(constant.TypeTwiddle, tag, []string{N.NetworkTCP}, options.DialerOptions),
		logger:  lg,
		dialer:  outboundDialer,
		server:  options.Server,
		port:    options.ServerPort,
		timeout: timeout,
		cred:    cred,
		cfg: tw.ClientConfig{
			Pool:     pool,
			CoverSNI: options.CoverSNI,
			Shaper:   tw.BrowsingShaper(false),
		},
	}, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if network != N.NetworkTCP {
		return nil, fmt.Errorf("twiddle: only TCP is supported")
	}
	o.logger.TraceContext(ctx, "dialing twiddle connection to ", o.server, ":", o.port)

	raw, err := o.dialer.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddrHostPort(o.server, o.port))
	if err != nil {
		return nil, fmt.Errorf("twiddle: TCP dial failed: %w", err)
	}
	raw.SetDeadline(time.Now().Add(o.timeout))

	cfg := o.cfg
	o.mu.Lock()
	cfg.Credential = o.cred
	o.mu.Unlock()

	conn, next, err := tw.Client(raw, cfg)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("twiddle: opening failed: %w", err)
	}
	raw.SetDeadline(time.Time{})

	// The egress issues the next credential inside its flight, exactly as
	// NewSessionTicket does. Rotating here keeps every ticket single-use, which
	// is what TLS 1.3 expects of a real client.
	if next != nil {
		o.mu.Lock()
		o.cred = next
		o.mu.Unlock()
	}

	if err := writeDestination(conn, destination.String()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func writeDestination(conn net.Conn, dest string) error {
	if len(dest) == 0 || len(dest) > 4096 {
		return fmt.Errorf("twiddle: destination length %d out of range", len(dest))
	}
	buf := make([]byte, 2+len(dest))
	buf[0] = byte(len(dest) >> 8)
	buf[1] = byte(len(dest))
	copy(buf[2:], dest)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("twiddle: failed to send destination: %w", err)
	}
	return nil
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("twiddle: UDP not supported")
}

func (o *Outbound) Network() []string { return []string{N.NetworkTCP} }
func (o *Outbound) Close() error      { return nil }
