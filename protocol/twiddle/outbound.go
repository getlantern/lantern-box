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
	"github.com/sagernet/sing/common/uot"

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

	// mu guards creds, the pool of credentials not yet spent. The egress issues
	// a fresh one inside every flight, so a completed connection returns more
	// than it took only in the sense that it replaces what it consumed.
	//
	// sing-box dials concurrently, which is why this is a pool and not a single
	// slot: each dial claims a distinct credential when one is available, so
	// sequential and moderately concurrent traffic never presents the same
	// ticket twice. Under a burst wider than the pool, dials fall back to
	// reusing the last credential rather than failing -- TLS 1.3 asks clients
	// not to reuse tickets, but a dead connection is worse than a reused one,
	// and the egress does not currently enforce single use. Eliminating reuse
	// entirely would need the egress to issue several credentials per flight.
	mu    sync.Mutex
	creds []*tw.Credential
	cfg   tw.ClientConfig

	// poolOrigin records which tier the hellos came from, so a client that has
	// quietly degraded to the built-in pool is diagnosable after the fact.
	poolOrigin tw.Origin
	uotClient  *uot.Client
}

// maxCredPool bounds credential accumulation on a long-lived outbound.
const maxCredPool = 32

func NewOutbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.TwiddleOutboundOptions) (adapter.Outbound, error) {
	ticket, err := base64.StdEncoding.DecodeString(options.Ticket)
	if err != nil {
		return nil, fmt.Errorf("twiddle: bad ticket: %w", err)
	}
	psk, err := hex.DecodeString(options.PSK)
	if err != nil {
		return nil, fmt.Errorf("twiddle: bad psk: %w", err)
	}
	// Optional: provisioning gained full_ticket after clients were already
	// deployed with ticket and psk alone, and a nil companion degrades to
	// resumption-only rather than failing.
	var fullTicket []byte
	if options.FullTicket != "" {
		if fullTicket, err = base64.StdEncoding.DecodeString(options.FullTicket); err != nil {
			return nil, fmt.Errorf("twiddle: bad full_ticket: %w", err)
		}
	}
	cred, err := tw.CredentialFromWireFull(ticket, fullTicket, psk)
	if err != nil {
		return nil, err
	}

	if options.CoverSNI == "" {
		return nil, fmt.Errorf("twiddle: cover_sni is required; it must agree with the egress's masquerade_upstream")
	}
	// An unmeasured cover is refused rather than approximated. Emitting a
	// plausible-looking profile for a host nobody measured is the failure this
	// is meant to prevent, not a degraded mode worth having.
	cover, err := tw.CoverFor(options.CoverSNI)
	if err != nil {
		return nil, err
	}
	// Ticket length is one of the cover's measured parameters, and the ticket
	// rides inside pre_shared_key, so a credential minted for another identity
	// produces a hello of the wrong size for the SNI it carries.
	if len(cred.Ticket) != cover.TicketLen {
		return nil, fmt.Errorf("twiddle: credential ticket length %d does not match cover %d", len(cred.Ticket), cover.TicketLen)
	}

	// Hello sourcing is twiddle's policy, not ours: it owns the precedence
	// (device tap, then config, then its built-in pool), the per-entry screening
	// and the partitioning of a source that mixes browser builds. Reimplementing
	// any of that here would let the two drift apart.
	pool, err := tw.LoadPool(tw.Sources{
		Device:       options.HelloPoolDevicePath,
		Config:       options.HelloPoolPath,
		ConfigInline: options.HelloPool,
		// The built-in pool is opt-in in the core because it is stale by
		// construction. It is enabled here for the reason argued at
		// TestOutboundDegradesToEmbeddedOnACorruptPool: a stale fingerprint on
		// every client beats no outbound on every client, and both need the same
		// config push to clear.
		AllowEmbedded: true,
	})
	if err != nil {
		return nil, err
	}
	// A client that silently drops to the built-in pool still connects, so this
	// has to be visible or a stale fingerprint ships unnoticed.
	for _, skipped := range pool.Skipped {
		lg.Warn("twiddle: ", skipped)
	}
	lg.Info("twiddle: ", len(pool.Hellos), " hellos from the ", pool.Origin, " pool")
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

	// The contact memory decides the opening shape per connection: full on
	// first contact with an egress, resumed afterwards, full again once a
	// censor can no longer be assumed to remember. Nil disables it entirely and
	// restores resumption-only behaviour.
	//
	// Safe to enable before anything else is ready. It degrades to resumption
	// unless ALL THREE of a probed full profile, a pool hello whose ECH payload
	// can carry the ticket, and a companion ticket are present -- and the cover
	// table ships no full profile at all, so today it always degrades. That is
	// deliberate: when per-egress probing and full_ticket provisioning land, the
	// path activates without another lantern-box release.
	var contacts *tw.ContactMemory
	if !options.DisableFullHandshake {
		contacts = tw.NewContactMemory(0, 0)
	}

	o := &Outbound{
		Adapter:    outbound.NewAdapterWithDialerOptions(constant.TypeTwiddle, tag, []string{N.NetworkTCP, N.NetworkUDP}, options.DialerOptions),
		logger:     lg,
		dialer:     outboundDialer,
		server:     options.Server,
		port:       options.ServerPort,
		timeout:    timeout,
		creds:      []*tw.Credential{cred},
		poolOrigin: pool.Origin,
		cfg: tw.ClientConfig{
			Pool:     pool.Hellos,
			Cover:    cover,
			Shaper:   tw.BrowsingShaper(false),
			Contacts: contacts,
		},
	}
	o.uotClient = &uot.Client{Dialer: o, Version: uot.Version}
	return o, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch N.NetworkName(network) {
	case N.NetworkUDP:
		return o.uotClient.DialContext(ctx, network, destination)
	case N.NetworkTCP:
	default:
		return nil, fmt.Errorf("twiddle: unsupported network: %s", network)
	}
	o.logger.TraceContext(ctx, "dialing twiddle connection to ", o.server, ":", o.port)

	raw, err := o.dialer.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddrHostPort(o.server, o.port))
	if err != nil {
		return nil, fmt.Errorf("twiddle: TCP dial failed: %w", err)
	}
	raw.SetDeadline(time.Now().Add(o.timeout))

	cfg := o.cfg
	cfg.Credential = o.takeCredential()

	conn, next, err := tw.Client(raw, cfg)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("twiddle: opening failed: %w", err)
	}
	raw.SetDeadline(time.Time{})

	// The egress issues the next credential inside its flight, exactly as
	// NewSessionTicket does.
	if next != nil {
		o.putCredential(next)
	}

	if err := writeDestination(conn, destination.String()); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// takeCredential claims a credential for one dial. The last one is reused
// rather than removed, so a dial never fails for want of a credential.
func (o *Outbound) takeCredential() *tw.Credential {
	o.mu.Lock()
	defer o.mu.Unlock()
	last := len(o.creds) - 1
	c := o.creds[last]
	if last > 0 {
		o.creds = o.creds[:last]
	}
	return c
}

func (o *Outbound) putCredential(c *tw.Credential) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.creds = append(o.creds, c)
	if len(o.creds) > maxCredPool {
		o.creds = o.creds[len(o.creds)-maxCredPool:]
	}
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
	return o.uotClient.ListenPacket(ctx, destination)
}

func (o *Outbound) Network() []string { return []string{N.NetworkTCP, N.NetworkUDP} }
func (o *Outbound) Close() error      { return nil }
