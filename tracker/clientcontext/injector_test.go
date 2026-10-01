package clientcontext

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	box "github.com/getlantern/lantern-box"
	lAdapter "github.com/getlantern/lantern-box/adapter"
	lconstant "github.com/getlantern/lantern-box/constant"
	"github.com/getlantern/lantern-box/protocol"
)

func TestEncodePayload(t *testing.T) {
	info := ClientInfo{DeviceID: "test-device", Platform: "test", IsPro: true, CountryCode: "US", Version: "1.0"}
	tests := []struct {
		name string
		ctx  context.Context
		want ClientInfo
	}{
		{name: "client info", ctx: context.Background(), want: info},
		// A probe carries no device info.
		{name: "probe", ctx: lAdapter.ContextWithProbe(context.Background())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := NewInjector(func() ClientInfo { return info }).encodePayload(tt.ctx)
			require.True(t, strings.HasPrefix(string(payload), clientInfoPrefix))
			var got ClientInfo
			require.NoError(t, json.Unmarshal(payload[len(clientInfoPrefix):], &got))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTags(t *testing.T) {
	injector := newTestInjector("a")
	assert.True(t, injector.shouldInject("a"))
	assert.False(t, injector.shouldInject("b"))

	injector.AddOutboundTags("b", "c")
	injector.RemoveOutboundTags("a", "c")
	assert.False(t, injector.shouldInject("a"))
	assert.True(t, injector.shouldInject("b"))
	assert.False(t, injector.shouldInject("c"))
}

func TestInstall(t *testing.T) {
	injector := newTestInjector()
	assert.Error(t, injector.Install(context.Background()), "no outbound registry")

	ctx := box.BaseContext()
	require.NoError(t, injector.Install(ctx))
	assert.Error(t, injector.Install(ctx), "already installed")
	assert.Error(t, NewInjector(injector.getInfo).Install(ctx), "already installed by another injector")

	// Protocols registered after Install still reach the wrapped registry.
	ctx = include.Context(context.Background())
	require.NoError(t, NewInjector(injector.getInfo).Install(ctx))
	protocol.RegisterProtocols(ctx)
	_, ok := service.FromContext[adapter.OutboundRegistry](ctx).CreateOptions(lconstant.TypeSamizdat)
	assert.True(t, ok)
}
