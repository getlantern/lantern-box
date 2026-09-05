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
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
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

	mu    sync.Mutex
	creds []*tw.Credential
	cfg   tw.ClientConfig

	sessMu sync.Mutex
	sess   *yamux.Session

	poolOrigin tw.Origin
	uotClient  *uot.Client
}

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
	cover, err := tw.CoverFor(options.CoverSNI)
	if err != nil {
		return nil, err
	}
	if len(cred.Ticket) != cover.TicketLen {
		return nil, fmt.Errorf("twiddle: credential ticket length %d does not match cover %d", len(cred.Ticket), cover.TicketLen)
	}

	// Embedded fallback is disabled: a stale compiled-in snapshot is a
	// fingerprint, and the right reaction is to fail this outbound so another
	// transport is selected.
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
	o.uotClient = &uot.Client{Dialer: (*uotDialer)(o), Version: uot.Version}
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
	return o.dialTunnel(ctx, destination)
}

// dialTunnel opens one multiplexed stream to destination on the shared outer
// connection, building that connection if there is not a live one.
//
// Both the TCP path and UoT's inner dial land here, which is the point: a UDP
// association rides the same tunnel as everything else rather than opening one
// of its own. A twiddle connection per association would put a fresh opening on
// the wire for every DNS lookup, which is the pattern muxing exists to remove.
func (o *Outbound) dialTunnel(ctx context.Context, destination M.Socksaddr) (net.Conn, error) {
	var openErr error
	for attempt := 0; attempt < 2; attempt++ {
		sess, err := o.ensureSession(ctx)
		if err != nil {
			return nil, err
		}
		stream, err := sess.Open()
		if err != nil {
			// GO_AWAY means the egress will take no NEW streams. The ones
			// already running on this tunnel are still live and still carrying
			// user traffic, and yamux's Close takes down the session AND every
			// stream on it -- so closing here would drop unrelated connections
			// because an unrelated dial arrived late. The session is retired
			// from the cache and left to drain instead; the peer closes the
			// socket once it is done, which shuts the session down on its own.
			// Any other Open failure means the session is already unusable, so
			// there is nothing to drain and the socket should go back now.
			o.retireSession(sess, !errors.Is(err, yamux.ErrRemoteGoAway))
			openErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if err := writeDestination(stream, destination.String()); err != nil {
			stream.Close()
			return nil, err
		}
		return stream, nil
	}
	return nil, fmt.Errorf("twiddle: open stream after replacing session: %w", openErr)
}

func (o *Outbound) ensureSession(ctx context.Context) (*yamux.Session, error) {
	o.sessMu.Lock()
	defer o.sessMu.Unlock()
	if o.sess != nil && !o.sess.IsClosed() {
		return o.sess, nil
	}
	o.logger.TraceContext(ctx, "opening twiddle tunnel to ", o.server, ":", o.port)

	cred := o.takeCredential()
	if cred == nil {
		return nil, fmt.Errorf("twiddle: no unused ticket; refusing to reuse")
	}

	raw, err := o.dialer.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddrHostPort(o.server, o.port))
	if err != nil {
		o.putCredential(cred)
		return nil, fmt.Errorf("twiddle: TCP dial failed: %w", err)
	}
	raw.SetDeadline(time.Now().Add(o.timeout))

	cfg := o.cfg
	cfg.Credential = cred
	conn, next, err := tw.Client(raw, cfg)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("twiddle: opening failed: %w", err)
	}
	raw.SetDeadline(time.Time{})
	if next != nil {
		o.putCredential(next)
	}

	sess, err := yamux.Client(conn, muxConfig())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("twiddle: mux: %w", err)
	}
	o.sess = sess
	return sess, nil
}

// uotDialer carries UoT's inner dial without routing it back through Outbound.
//
// uot.Client needs an N.Dialer and Outbound is one, but wiring it to itself
// makes both DialContext(udp) and ListenPacket self-referential: their
// termination then rests on uot.Client never handing UDP back to DialContext
// and never calling Dialer.ListenPacket at all. Both hold in sing v0.8.13 --
// and both are upstream details that a dependency bump could change, at which
// point the failure is an unbounded recursion in production rather than a
// compile error. Cutting the loop here costs nothing and matches how unbounded,
// samizdat and water each wire their own UoT client.
//
// It reaches dialTunnel, so UoT's inner connection is a stream on the shared
// session rather than a tunnel of its own.
type uotDialer Outbound

func (d *uotDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkTCP {
		return nil, fmt.Errorf("twiddle: uot inner dial must be TCP, got %q: %w", network, os.ErrInvalid)
	}
	return (*Outbound)(d).dialTunnel(ctx, destination)
}

func (d *uotDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, fmt.Errorf("twiddle: uotDialer does not support ListenPacket: %w", os.ErrInvalid)
}

// retireSession removes sess as the cached tunnel so the next dial builds a
// fresh one. It closes sess only when closeIt is set -- see the GO_AWAY note in
// dialTunnel for why that is not always wanted.
func (o *Outbound) retireSession(sess *yamux.Session, closeIt bool) {
	o.sessMu.Lock()
	defer o.sessMu.Unlock()
	if o.sess != sess {
		return
	}
	o.sess = nil
	if closeIt {
		sess.Close()
	}
}

// takeCredential claims one unused ticket. It never reuses: a parallel burst
// with one ticket opens one tunnel (via ensureSession's lock) and the rest
// ride streams. An exhausted pool fails rather than emitting the same PSK
// identity on two connections.
func (o *Outbound) takeCredential() *tw.Credential {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.creds) == 0 {
		return nil
	}
	last := len(o.creds) - 1
	c := o.creds[last]
	o.creds = o.creds[:last]
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

func (o *Outbound) Close() error {
	o.sessMu.Lock()
	defer o.sessMu.Unlock()
	if o.sess != nil {
		err := o.sess.Close()
		o.sess = nil
		return err
	}
	return nil
}
