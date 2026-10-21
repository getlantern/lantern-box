package uotrouter

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/uot"
	"github.com/stretchr/testify/require"
)

type recordingRouter struct {
	routed []adapter.InboundContext
	packet []bool
}

func (r *recordingRouter) RouteConnection(_ context.Context, _ net.Conn, metadata adapter.InboundContext) error {
	r.routed, r.packet = append(r.routed, metadata), append(r.packet, false)
	return nil
}

func (r *recordingRouter) RoutePacketConnection(_ context.Context, _ N.PacketConn, metadata adapter.InboundContext) error {
	r.routed, r.packet = append(r.routed, metadata), append(r.packet, true)
	return nil
}

func (r *recordingRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	_ = r.RouteConnection(ctx, conn, metadata)
}

func (r *recordingRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	_ = r.RoutePacketConnection(ctx, conn, metadata)
}

func pipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// uotConn returns the server end of a pipe whose client end has already sent
// a UoT request for destination, as a UoT client would after the tunnel opens.
func uotConn(t *testing.T, destination M.Socksaddr) net.Conn {
	t.Helper()
	client, server := pipe(t)
	go func() { _ = uot.WriteRequest(client, uot.Request{Destination: destination}) }()
	return server
}

func routeBoth(t *testing.T, conn func() net.Conn, metadata adapter.InboundContext) []adapter.InboundContext {
	t.Helper()
	inner := &recordingRouter{}
	r := New(inner, log.NewNOPFactory().Logger())
	require.NoError(t, r.RouteConnection(context.Background(), conn(), metadata))
	r.RouteConnectionEx(context.Background(), conn(), metadata, nil)
	require.Len(t, inner.routed, 2)
	return inner.routed
}

func TestMagicAddressExposesRealDomain(t *testing.T) {
	dst := M.ParseSocksaddrHostPort("i.instagram.com", 443)
	matcher, err := rule.NewDomainItem(nil, []string{"instagram.com"})
	require.NoError(t, err)

	routed := routeBoth(t, func() net.Conn { return uotConn(t, dst) }, adapter.InboundContext{
		Destination: M.Socksaddr{Fqdn: uot.MagicAddress},
	})
	for _, md := range routed {
		require.Empty(t, md.Domain)
		require.Equal(t, "i.instagram.com", md.Destination.Fqdn)
		require.Equal(t, uint16(443), md.Destination.Port)
		require.True(t, matcher.Match(&md), "domain_suffix rule must see the real host")
	}
}

func TestLegacyMagicAddressClearsDomain(t *testing.T) {
	routed := routeBoth(t, func() net.Conn {
		_, s := pipe(t)
		return s
	}, adapter.InboundContext{Destination: M.Socksaddr{Fqdn: uot.LegacyMagicAddress}})
	for _, md := range routed {
		require.Empty(t, md.Domain)
	}
}

func TestNonUoTPassesThroughUnchanged(t *testing.T) {
	in := adapter.InboundContext{
		Domain:      "sniffed.example.com",
		Destination: M.ParseSocksaddrHostPort("example.com", 443),
	}
	inner := &recordingRouter{}
	r := New(inner, log.NewNOPFactory().Logger())
	_, s := pipe(t)

	require.NoError(t, r.RouteConnection(context.Background(), s, in))
	r.RouteConnectionEx(context.Background(), s, in, nil)
	require.NoError(t, r.RoutePacketConnection(context.Background(), nil, in))
	r.RoutePacketConnectionEx(context.Background(), nil, in, nil)

	require.Equal(t, []bool{false, false, true, true}, inner.packet)
	for _, md := range inner.routed {
		require.Equal(t, in, md)
	}
}
