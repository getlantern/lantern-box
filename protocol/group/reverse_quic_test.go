package group

import (
	"context"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/urltest"
	sbLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/constant"
	isync "github.com/getlantern/lantern-box/internal/sync"
	"github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"
)

// A samizdat-reverse member black-holes QUIC rather than failing it, so a
// QUIC attempt never costs the member its URL-test history or selection.
// Without a session, UDP to any other port fails and does cost it both.
func TestMutableURLTest_ReverseQUICKeepsHistory(t *testing.T) {
	for _, tc := range []struct {
		port        uint16
		keepHistory bool
	}{
		{443, true},
		{3478, false},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reverse, err := samizdatreverse.NewReverseOutbound(ctx, nil, sbLog.NewNOPFactory().Logger(), "reverse",
			option.SamizdatReverseOutboundOptions{Portal: "portal"})
		require.NoError(t, err)
		other := &mockOutbound{tag: "other"}
		other.On("ListenPacket").Return(nil, assert.AnError)

		g := &urlTestGroup{
			ctx:       ctx,
			logger:    sbLog.NewNOPFactory().Logger(),
			tags:      []string{"reverse", "other"},
			outbounds: isync.TypedMap[string, adapter.Outbound]{},
			tolerance: 50,
			history:   urltest.NewHistoryStorage(),
		}
		g.outbounds.Store("reverse", reverse)
		g.outbounds.Store("other", other)
		g.history.StoreURLTestHistory("reverse", &adapter.URLTestHistory{Time: time.Now(), Delay: 50})
		g.history.StoreURLTestHistory("other", &adapter.URLTestHistory{Time: time.Now(), Delay: 1000})
		g.selectedOutboundUDP.Store(reverse)
		s := &MutableURLTest{
			Adapter: O.NewAdapter(constant.TypeMutableURLTest, "test", []string{"tcp", "udp"}, nil),
			ctx:     ctx,
			logger:  sbLog.NewNOPFactory().Logger(),
			group:   g,
		}

		pc, err := s.ListenPacket(ctx, metadata.ParseSocksaddrHostPort("10.11.12.13", tc.port))
		if tc.keepHistory {
			require.NoError(t, err, "port %d", tc.port)
			pc.Close()
			assert.NotNil(t, g.history.LoadURLTestHistory("reverse"), "port %d", tc.port)
			assert.Equal(t, "reverse", g.selectedOutboundUDP.Load().Tag(), "port %d", tc.port)
		} else {
			require.Error(t, err, "port %d", tc.port)
			assert.Nil(t, g.history.LoadURLTestHistory("reverse"), "port %d", tc.port)
		}
	}
}
