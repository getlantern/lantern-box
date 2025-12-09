package box

import (
	"context"

	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/lantern-box/protocol"
)

type PlatformInterface interface {
	LocalDNSTransport() libbox.LocalDNSTransport
}

// BaseContext returns a context with all sing-box and lantern-box protocols and DNS transports registered.
func BaseContext() context.Context {
	return BaseContextWithDNSTransport(nil)
}

// BaseContextWithDNSTransport returns the [BaseContext] and registers the [libbox.LocalDNSTransport]
// provided by the given [PlatformInterface].
func BaseContextWithDNSTransport(platformInterface PlatformInterface) context.Context {
	var pi libbox.PlatformInterface
	if platformInterface != nil {
		pi = &platformAdapter{pltIfc: platformInterface}
	}
	ctx := libbox.BaseContext(pi)
	return protocol.RegisterProtocols(ctx)
}

// platformAdapter is a helper to adapt PlatformInterface to libbox.PlatformInterface and satisfy
// libbox.BaseContext, which only uses LocalDNSTransport. This avoids misleading callers into thinking
// they need to implement the full libbox.PlatformInterface when only LocalDNSTransport is required.
type platformAdapter struct {
	libbox.PlatformInterface
	pltIfc PlatformInterface
}

func (p *platformAdapter) LocalDNSTransport() libbox.LocalDNSTransport {
	return p.pltIfc.LocalDNSTransport()
}
