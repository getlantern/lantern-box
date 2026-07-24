package group

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	sbAdapter "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/adapter"
	lLog "github.com/getlantern/lantern-box/log"
)

func TestMutableAutoSelect_RejectsRecursiveTUNDial(t *testing.T) {
	s, outbounds := newTestMUR(t, "proxy-a", "proxy-b")
	s.recordProbeOutcome("proxy-a", true, 10)
	s.recordProbeOutcome("proxy-b", true, 20)
	before := s.history.All()
	endpoint := netip.MustParseAddrPort("192.0.2.10:443")
	s.physicalEndpoints = adapter.NewPhysicalEndpointRegistry()
	s.physicalEndpoints.Set("proxy-a", []netip.AddrPort{endpoint})

	var logs bytes.Buffer
	s.logger = lLog.NewFactory(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})).Logger()
	ctx := sbAdapter.WithContext(context.Background(), &sbAdapter.InboundContext{InboundType: C.TypeTun})

	for range 3 {
		conn, err := s.DialContext(ctx, "tcp", M.SocksaddrFromNetIP(endpoint))
		require.Nil(t, conn)
		var loopErr *adapter.ProxyRoutingLoopError
		require.ErrorAs(t, err, &loopErr)
		assert.ErrorIs(t, err, adapter.ErrProxyRoutingLoop)
		assert.Equal(t, "proxy-a", loopErr.OutboundTag)
		assert.NotContains(t, err.Error(), endpoint.Addr().String())
	}

	outbounds["proxy-a"].AssertNumberOfCalls(t, "DialContext", 0)
	outbounds["proxy-b"].AssertNumberOfCalls(t, "DialContext", 0)
	time.Sleep(20 * time.Millisecond)
	assert.False(t, s.laddering.Load())
	assert.Zero(t, s.lastLadderAt.Load())
	assert.Equal(t, before, s.history.All(), "recursive dials must not change selection history")

	output := logs.String()
	assert.Equal(t, 1, strings.Count(output, "proxy routing loop rejected"))
	assert.Contains(t, output, "proxy-a")
	assert.NotContains(t, output, endpoint.Addr().String())
}

func TestMutableAutoSelect_EmptyRegistryPreservesDialBehavior(t *testing.T) {
	s, outbounds := newTestMUR(t, "proxy-a")
	s.recordProbeOutcome("proxy-a", true, 10)
	s.physicalEndpoints = adapter.NewPhysicalEndpointRegistry()
	endpoint := netip.MustParseAddrPort("192.0.2.10:443")
	outbounds["proxy-a"].On("DialContext").Return(echoConn{}, nil).Once()
	ctx := sbAdapter.WithContext(context.Background(), &sbAdapter.InboundContext{InboundType: C.TypeTun})

	conn, err := s.DialContext(ctx, "tcp", M.SocksaddrFromNetIP(endpoint))

	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, conn.Close())
	outbounds["proxy-a"].AssertNumberOfCalls(t, "DialContext", 1)
}

func TestMutableAutoSelect_CircuitBreakerIsTUNScoped(t *testing.T) {
	s, outbounds := newTestMUR(t, "proxy-a")
	s.recordProbeOutcome("proxy-a", true, 10)
	endpoint := netip.MustParseAddrPort("192.0.2.10:443")
	s.physicalEndpoints = adapter.NewPhysicalEndpointRegistry()
	s.physicalEndpoints.Set("proxy-a", []netip.AddrPort{endpoint})
	outbounds["proxy-a"].On("DialContext").Return(echoConn{}, nil).Once()

	conn, err := s.DialContext(context.Background(), "tcp", M.SocksaddrFromNetIP(endpoint))

	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, conn.Close())
	outbounds["proxy-a"].AssertNumberOfCalls(t, "DialContext", 1)
}

func TestMutableAutoSelect_RejectsRecursiveAlternate(t *testing.T) {
	s, outbounds := newTestMUR(t, "proxy-a", "proxy-b")
	s.recordProbeOutcome("proxy-a", true, 10)
	s.recordProbeOutcome("proxy-b", true, 20)
	endpoint := netip.MustParseAddrPort("192.0.2.20:443")
	s.physicalEndpoints = adapter.NewPhysicalEndpointRegistry()
	s.physicalEndpoints.Set("proxy-b", []netip.AddrPort{endpoint})
	beforeB := s.history.Load("proxy-b")
	outbounds["proxy-a"].On("DialContext").Return(nil, errors.New("proxy refused connection")).Once()
	ctx := sbAdapter.WithContext(context.Background(), &sbAdapter.InboundContext{InboundType: C.TypeTun})

	conn, err := s.DialContext(ctx, "tcp", M.SocksaddrFromNetIP(endpoint))

	require.Nil(t, conn)
	require.ErrorIs(t, err, adapter.ErrProxyRoutingLoop)
	outbounds["proxy-a"].AssertNumberOfCalls(t, "DialContext", 1)
	outbounds["proxy-b"].AssertNumberOfCalls(t, "DialContext", 0)
	assert.Len(t, s.history.Load("proxy-a").UserFailures, 1)
	assert.Equal(t, beforeB, s.history.Load("proxy-b"))
	time.Sleep(20 * time.Millisecond)
	assert.Zero(t, s.lastLadderAt.Load())
}

func TestMutableAutoSelect_RejectsRecursivePacketDial(t *testing.T) {
	s, outbounds := newTestMUR(t, "proxy-a")
	s.recordProbeOutcome("proxy-a", true, 10)
	endpoint := netip.MustParseAddrPort("192.0.2.10:443")
	s.physicalEndpoints = adapter.NewPhysicalEndpointRegistry()
	s.physicalEndpoints.Set("proxy-a", []netip.AddrPort{endpoint})
	ctx := sbAdapter.WithContext(context.Background(), &sbAdapter.InboundContext{InboundType: C.TypeTun})

	conn, err := s.ListenPacket(ctx, M.SocksaddrFromNetIP(endpoint))

	require.Nil(t, conn)
	require.ErrorIs(t, err, adapter.ErrProxyRoutingLoop)
	outbounds["proxy-a"].AssertNumberOfCalls(t, "ListenPacket", 0)
}
