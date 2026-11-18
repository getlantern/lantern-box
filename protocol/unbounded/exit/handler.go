//go:build with_quic

package exit

import (
	"context"
	"net"
	"os"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/getlantern/lantern-box/protocol/internal/failureclass"
)

// handler routes each of a session's streams and flows through its kind's router current at
// that moment, so that a stream opened after the exit box rotates goes to the new one.
type handler struct {
	exit *Exit
	kind Kind
}

func (h *handler) NewConnectionEx(ctx context.Context, conn net.Conn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	r := h.exit.currentRouter(h.kind)
	if r == nil {
		_ = N.CloseOnHandshakeFailure(conn, onClose, os.ErrClosed)
		return
	}
	ctx = log.ContextWithNewID(ctx)
	r.RouteConnectionEx(ctx, h.exit.metrics.countConn(conn), metadata(source, destination), h.exit.onRoutedClose(ctx, onClose))
}

func (h *handler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	r := h.exit.currentRouter(h.kind)
	if r == nil {
		_ = N.CloseOnHandshakeFailure(conn, onClose, os.ErrClosed)
		return
	}
	ctx = log.ContextWithNewID(ctx)
	r.RoutePacketConnectionEx(ctx, h.exit.metrics.countPacketConn(conn), metadata(source, destination), h.exit.onRoutedClose(ctx, onClose))
}

// metadata is a stream's or flow's routing metadata. source is the session's placeholder
// address, never the user's.
func metadata(source, destination M.Socksaddr) adapter.InboundContext {
	var md adapter.InboundContext
	md.Inbound = InboundTag
	md.InboundType = "unbounded"
	md.Source = source
	md.Destination = destination
	return md
}

// onRoutedClose counts a refusal and logs the failure's class, never its destination.
func (e *Exit) onRoutedClose(ctx context.Context, onClose N.CloseHandlerFunc) N.CloseHandlerFunc {
	return func(err error) {
		if err != nil {
			if rule.IsRejected(err) {
				e.metrics.refused()
			}
			e.logger.DebugContext(ctx, "unbounded exit: a routed connection failed: ", failureclass.Of(err))
		}
		if onClose != nil {
			onClose(err)
		}
	}
}
