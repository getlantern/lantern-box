package box

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	sbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/constant"
)

// TestDNSPoolRegistered checks every context lantern-box builds knows the
// pool DNS server type.
func TestDNSPoolRegistered(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"Context":                     Context(context.Background()),
		"ContextWithDNSTransport":     ContextWithDNSTransport(context.Background(), stubPlatform{transport: stubLocalDNSTransport{}}),
		"BaseContext":                 BaseContext(),
		"BaseContextWithDNSTransport": BaseContextWithDNSTransport(stubPlatform{transport: stubLocalDNSTransport{}}),
	} {
		t.Run(name, func(t *testing.T) {
			registry := service.FromContext[adapter.DNSTransportRegistry](ctx)
			require.NotNil(t, registry)
			_, ok := registry.CreateOptions(constant.TypeDNSPool)
			assert.True(t, ok)
		})
	}
}

// TestDNSPoolInABox runs a pool from JSON config in a real box: the first
// server is a local server on a port nothing answers on, so the pool must
// answer from the second.
func TestDNSPoolInABox(t *testing.T) {
	ctx := service.ContextWithDefaultRegistry(Context(t.Context()))
	options, err := json.UnmarshalExtendedContext[option.Options](ctx, []byte(`{
		"log": {"disabled": true},
		"dns": {
			"servers": [
				{"type": "udp", "tag": "dead", "server": "127.0.0.1", "server_port": 9},
				{"type": "hosts", "tag": "hosts", "predefined": {"example.com": "192.0.2.7"}},
				{"type": "pool", "tag": "pool", "servers": ["dead", "hosts"], "stagger": "50ms", "timeout": "1s"}
			],
			"final": "pool"
		}
	}`))
	require.NoError(t, err)
	instance, err := sbox.New(sbox.Options{Context: ctx, Options: options})
	require.NoError(t, err)
	require.NoError(t, instance.Start())
	defer instance.Close()

	manager := service.FromContext[adapter.DNSTransportManager](ctx)
	require.NotNil(t, manager)
	pool, ok := manager.Transport("pool")
	require.True(t, ok)
	query := new(dns.Msg)
	query.SetQuestion("example.com.", dns.TypeA)
	exchangeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response, err := pool.Exchange(exchangeCtx, query)
	require.NoError(t, err)
	require.Len(t, response.Answer, 1)
	assert.Equal(t, "192.0.2.7", response.Answer[0].(*dns.A).A.String())
}
