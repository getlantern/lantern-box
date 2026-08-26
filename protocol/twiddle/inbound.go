package twiddle

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
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
			PSKFirst:  options.PSKFirst,
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
	// Stop recording either way. On success the session continues reading through
	// peekConn, and without this every byte of an authenticated connection would
	// be buffered for the lifetime of the session.
	peeked.stop()
	if err != nil {
		i.logger.DebugContext(ctx, "twiddle: unauthenticated connection from ", metadata.Source,
			"; masquerading to ", i.masqueradeUpstream)
		ferr := masquerade.Forward(ctx, conn, i.masqueradeUpstream, peeked.replay())
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

// peekConn records what is read during authentication so a failed attempt can be
// replayed to the cover site byte for byte. A prober must see its own bytes
// arrive at a real server, not a truncated stream.
//
// Recording is bounded twice over. stop() ends it as soon as authentication
// resolves -- otherwise an authenticated session would buffer its entire
// lifetime's traffic -- and maxPeek caps what an unauthenticated peer can make
// us hold before it resolves, since a prober controls how much it sends.
type peekConn struct {
	mu      sync.Mutex
	stopped bool
	seen    []byte
	net.Conn
}

// maxPeek bounds the replay buffer. A ClientHello is ~2 KB; 64 KB leaves room
// for a peer whose opening spans several records without letting one hold
// unbounded memory.
const maxPeek = 64 << 10

func (c *peekConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.mu.Lock()
		if !c.stopped && len(c.seen) < maxPeek {
			room := maxPeek - len(c.seen)
			if room > n {
				room = n
			}
			c.seen = append(c.seen, b[:room]...)
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *peekConn) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

func (c *peekConn) replay() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
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
