package clientcontext

import (
	"context"
	"errors"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/service"

	isync "github.com/getlantern/lantern-box/internal/sync"
)

// Injector sends [ClientInfo] to the server on connections dialed through
// outbounds enabled for injection. Dials marked with
// [github.com/getlantern/lantern-box/adapter.ContextWithProbe] exchange the
// zero ClientInfo and are not attributed to a client. It is safe for
// concurrent use.
type Injector struct {
	getInfo      GetClientInfoFn
	outboundTags isync.TypedMap[string, struct{}]
}

// NewInjector returns an Injector that sends getInfo() on dials through
// outbounds whose tag is in outboundTags. Their peers must support the
// client-info exchange.
func NewInjector(getInfo GetClientInfoFn, outboundTags ...string) *Injector {
	i := &Injector{getInfo: getInfo}
	i.AddOutboundTags(outboundTags...)
	return i
}

// AddOutboundTags enables injection for the given outbound tags. Their
// peers must support the client-info exchange. It applies from the next
// dial; open connections are unaffected.
func (i *Injector) AddOutboundTags(tags ...string) {
	for _, tag := range tags {
		i.outboundTags.Store(tag, struct{}{})
	}
}

// RemoveOutboundTags disables injection for the given outbound tags.
// It applies from the next dial; connections already open are unaffected.
func (i *Injector) RemoveOutboundTags(tags ...string) {
	for _, tag := range tags {
		i.outboundTags.Delete(tag)
	}
}

func (i *Injector) shouldInject(tag string) bool {
	_, ok := i.outboundTags.Load(tag)
	return ok
}

// Install replaces the outbound registry in ctx with one whose outbounds send
// client info through i. It must be called after the registries are added to
// ctx (e.g. by box.Context) and before box.New. It returns an error if ctx has
// no outbound registry or already has an Injector installed.
func (i *Injector) Install(ctx context.Context) error {
	registry := service.FromContext[adapter.OutboundRegistry](ctx)
	if registry == nil {
		return errors.New("clientcontext: no outbound registry in context")
	}
	// A second wrapper would send client info twice, and the server forwards
	// the second copy to the destination.
	if _, installed := common.Cast[*outboundRegistry](registry); installed {
		return errors.New("clientcontext: injector already installed")
	}
	service.MustRegister[adapter.OutboundRegistry](ctx, &outboundRegistry{
		OutboundRegistry: registry,
		injector:         i,
	})
	return nil
}
