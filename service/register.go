// Package service holds lantern-box's sing-box services and their
// registration.
package service

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	singService "github.com/sagernet/sing/service"

	"github.com/getlantern/lantern-box/service/outboundeval"
)

// RegisterServices registers all lantern-box services to the given context's
// service registry. A context without one is returned unchanged.
// Note: this does not register sing-box built-in services.
func RegisterServices(ctx context.Context) context.Context {
	if registry := singService.FromContext[adapter.ServiceRegistry](ctx); registry != nil {
		if reg, ok := registry.(*boxService.Registry); ok {
			registerServices(reg)
		}
	}
	return ctx
}

// ***** REGISTER NEW SERVICES HERE ***** //

func registerServices(registry *boxService.Registry) {
	outboundeval.RegisterService(registry)
}
