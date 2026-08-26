package twiddle

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/internal/masquerade"
	tw "github.com/getlantern/twiddle"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.TwiddleInboundOptions](registry, constant.TypeTwiddle, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	ctx                context.Context
	logger             log.ContextLogger
	router             adapter.Router
	listener           *listener.Listener
	cfg                tw.ServerConfig
	masqueradeUpstream string
}

func NewInbound(ctx context.Context, router adapter.Router, lg log.ContextLogger, tag string, options option.TwiddleInboundOptions) (adapter.Inbound, error) {
	raw, err := hex.DecodeString(options.TicketKey)
	if err != nil {
		return nil, fmt.Errorf("twiddle: bad ticket_key: %w", err)
	}
	key, err := tw.TicketKeyFromWire(raw)
	if err != nil {
		return nil, err
	}
	if options.MasqueradeUpstream == "" {
		return nil, errors.New("twiddle: masquerade_upstream is required; without it an active prober gets a distinguishing reply")
	}

	var maxAge time.Duration
	if options.TicketMaxAge != "" {
		if maxAge, err = time.ParseDuration(options.TicketMaxAge); err != nil {
			return nil, fmt.Errorf("twiddle: invalid ticket_max_age: %w", err)
		}
	}

	ib := &Inbound{
		Adapter: inbound.NewAdapter(constant.TypeTwiddle, tag),
		ctx:     ctx,
		logger:  lg,
		router:  router,
		cfg: tw.ServerConfig{
			TicketKey: key,
			MaxAge:    maxAge,
			TicketLen: options.TicketLen,
			Shaper:    tw.BrowsingShaper(true),
		},
		masqueradeUpstream: options.MasqueradeUpstream,
	}
	ib.listener = listener.New(listener.Options{
		Context: ctx, Logger: lg, Network: []string{N.NetworkTCP},
		Listen: options.ListenOptions, ConnectionHandler: ib,
	})
	return ib, nil
}

// NewConnectionEx authenticates the opening, or hands the connection to the
// cover site.
//
// twiddle answers a harvested ClientHello with a synthesised ServerHello, so it
// cannot complete a genuine handshake with a peer it does not recognise. Every
// unauthenticated connection therefore MUST reach a real server -- an active
// prober that gets anything else has confirmed the transport.
func (i *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	peeked := &peekConn{Conn: conn}
	tconn, err := tw.Server(peeked, i.cfg)
	if err != nil {
		i.logger.DebugContext(ctx, "twiddle: unauthenticated connection from ", metadata.Source,
			"; masquerading to ", i.masqueradeUpstream)
		ferr := masquerade.Forward(ctx, conn, i.masqueradeUpstream, peeked.seen)
		if ferr != nil {
			i.logger.DebugContext(ctx, "twiddle: masquerade forward error: ", ferr)
		}
		conn.Close()
		if onClose != nil {
			onClose(ferr)
		}
		return
	}

	dest, err := readDestination(tconn)
	if err != nil {
		N.CloseOnHandshakeFailure(tconn, onClose, err)
		return
	}
	destination := M.ParseSocksaddr(dest)
	i.logger.TraceContext(ctx, "twiddle connection from ", metadata.Source, " to ", destination)

	metadata.Inbound = i.Tag()
	metadata.InboundType = constant.TypeTwiddle
	metadata.Destination = destination
	i.router.RouteConnectionEx(ctx, tconn, metadata, onClose)
}

func readDestination(r io.Reader) (string, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", fmt.Errorf("twiddle: read destination length: %w", err)
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	if n == 0 || n > 4096 {
		return "", fmt.Errorf("twiddle: invalid destination length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("twiddle: read destination: %w", err)
	}
	return string(buf), nil
}

// peekConn records everything read from the connection so that a failed
// authentication can be replayed to the cover site byte for byte. A prober must
// see its own bytes arrive at a real server, not a truncated stream.
type peekConn struct {
	net.Conn
	seen []byte
	done bool
}

func (c *peekConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && !c.done {
		c.seen = append(c.seen, b[:n]...)
	}
	return n, err
}

func (i *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	return i.listener.Start()
}

func (i *Inbound) Close() error {
	if i.listener != nil {
		return i.listener.Close()
	}
	return nil
}
