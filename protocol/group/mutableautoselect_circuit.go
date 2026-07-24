package group

import (
	"context"
	"fmt"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/getlantern/lantern-box/adapter"
)

const routingLoopDiagnosticInterval = time.Minute

// rejectRecursiveDial stops traffic captured by the TUN from re-entering the
// selected outbound through that outbound's own physical endpoint.
func (s *MutableAutoSelect) rejectRecursiveDial(ctx context.Context, outboundTag string, destination M.Socksaddr) error {
	inbound := A.ContextFrom(ctx)
	if inbound == nil || inbound.InboundType != C.TypeTun || !destination.IsIP() || s.physicalEndpoints == nil {
		return nil
	}
	if !s.physicalEndpoints.Contains(outboundTag, destination.Unwrap().AddrPort()) {
		return nil
	}

	err := &adapter.ProxyRoutingLoopError{OutboundTag: outboundTag}
	s.emitRoutingLoopDiagnostic(ctx, outboundTag)
	return err
}

func (s *MutableAutoSelect) emitRoutingLoopDiagnostic(ctx context.Context, outboundTag string) {
	now := time.Now()
	for {
		last := s.lastRoutingLoopDiagnostic.Load()
		if last != 0 && now.Sub(time.Unix(0, last)) < routingLoopDiagnosticInterval {
			return
		}
		if s.lastRoutingLoopDiagnostic.CompareAndSwap(last, now.UnixNano()) {
			s.logger.WarnContext(ctx, fmt.Sprintf("proxy routing loop rejected: outbound=%q", outboundTag))
			return
		}
	}
}
