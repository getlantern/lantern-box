package box

import (
	"context"

	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/protocol"
)

// Context returns a context with all sing-box and lantern-box registries.
func Context(ctx context.Context) context.Context {
	return ContextWithDNSTransport(ctx, nil)
}

// ContextWithDNSTransport returns [Context], with `local` DNS servers resolved through the
// [libbox.LocalDNSTransport] of platformInterface when it provides one, unless ctx already
// carries a DNS transport registry. Unlike [BaseContextWithDNSTransport], it derives from ctx
// and installs no file manager.
func ContextWithDNSTransport(ctx context.Context, platformInterface PlatformInterface) context.Context {
	var dnsRegistry adapter.DNSTransportRegistry = include.DNSTransportRegistry()
	if platformInterface != nil {
		if transport := platformInterface.LocalDNSTransport(); transport != nil {
			dnsRegistry = service.FromContext[adapter.DNSTransportRegistry](
				libbox.BaseContext(&platformAdapter{transport: transport}),
			)
		}
	}
	ctx = sbox.Context(ctx,
		include.InboundRegistry(),
		include.OutboundRegistry(),
		include.EndpointRegistry(),
		dnsRegistry,
		include.ServiceRegistry(),
		include.CertificateProviderRegistry(),
	)
	return protocol.RegisterProtocols(ctx)
}

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
		if transport := platformInterface.LocalDNSTransport(); transport != nil {
			pi = &platformAdapter{transport: transport}
		}
	}
	return protocol.RegisterProtocols(libbox.BaseContext(pi))
}

// platformAdapter satisfies libbox.PlatformInterface for libbox.BaseContext, which calls only
// LocalDNSTransport; any other method panics on the nil embedded interface.
type platformAdapter struct {
	libbox.PlatformInterface
	transport libbox.LocalDNSTransport
}

func (p *platformAdapter) LocalDNSTransport() libbox.LocalDNSTransport {
	return p.transport
}
