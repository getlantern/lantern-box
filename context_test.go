package box

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubLocalDNSTransport struct{ exchanges chan struct{} }

func (stubLocalDNSTransport) Raw() bool                                            { return true }
func (stubLocalDNSTransport) Lookup(*libbox.ExchangeContext, string, string) error { return nil }
func (s stubLocalDNSTransport) Exchange(*libbox.ExchangeContext, []byte) error {
	s.exchanges <- struct{}{}
	return nil
}

type stubPlatform struct{ transport libbox.LocalDNSTransport }

func (p stubPlatform) LocalDNSTransport() libbox.LocalDNSTransport { return p.transport }

type ctxKey struct{}

func localTransport(t *testing.T, ctx context.Context) adapter.DNSTransport {
	t.Helper()
	registry := service.FromContext[adapter.DNSTransportRegistry](ctx)
	require.NotNil(t, registry)
	transport, err := registry.CreateDNSTransport(ctx, log.NewNOPFactory().NewLogger("dns"), "local", C.DNSTypeLocal, &option.LocalDNSServerOptions{})
	require.NoError(t, err)
	return transport
}

func localTransportType(t *testing.T, ctx context.Context) string {
	t.Helper()
	return fmt.Sprintf("%T", localTransport(t, ctx))
}

func TestContextWithDNSTransport(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxKey{}, "parent")

	t.Run("platform transport", func(t *testing.T) {
		stub := stubLocalDNSTransport{exchanges: make(chan struct{}, 1)}
		ctx := ContextWithDNSTransport(parent, stubPlatform{transport: stub})
		assert.Equal(t, "parent", ctx.Value(ctxKey{}))
		assert.Nil(t, service.FromContext[filemanager.Manager](ctx))
		transport := localTransport(t, ctx)
		assert.Equal(t, "*libbox.platformTransport", fmt.Sprintf("%T", transport))

		query := new(dns.Msg)
		query.SetQuestion("example.com.", dns.TypeA)
		exchangeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := transport.Exchange(exchangeCtx, query)
		require.NoError(t, err)
		select {
		case <-stub.exchanges:
		default:
			t.Fatal("platform DNS transport was not called")
		}
	})
	t.Run("platform without transport", func(t *testing.T) {
		ctx := ContextWithDNSTransport(parent, stubPlatform{})
		assert.Equal(t, "*local.Transport", localTransportType(t, ctx))
	})
	t.Run("no platform", func(t *testing.T) {
		ctx := Context(parent)
		assert.Equal(t, "parent", ctx.Value(ctxKey{}))
		assert.Equal(t, "*local.Transport", localTransportType(t, ctx))
	})
}
