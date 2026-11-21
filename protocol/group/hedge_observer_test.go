package group

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"

	lAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

type connLabelKey struct{}

type recordingHedgeObserver struct {
	mu     sync.Mutex
	labels []string
}

func (o *recordingHedgeObserver) FirstCommitted(ctx context.Context) {
	label, _ := ctx.Value(connLabelKey{}).(string)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.labels = append(o.labels, label)
}

func (o *recordingHedgeObserver) committed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.labels...)
}

func newObservedHedgeFixture(t *testing.T, first, then *fakeOutbound) (*hedgeFixture, *recordingHedgeObserver) {
	t.Helper()
	first.tag, then.tag = "first", "then"
	mgr := &mockOutboundManager{outbounds: map[string]adapter.Outbound{"first": first, "then": then}}
	ctx := service.ContextWithDefaultRegistry(context.Background())
	service.MustRegister[adapter.OutboundManager](ctx, mgr)
	obs := &recordingHedgeObserver{}
	service.MustRegister[lAdapter.HedgeObserver](ctx, obs)
	opts := option.HedgeOutboundOptions{First: "first", Then: "then", CommitTimeout: badoption.Duration(100 * time.Millisecond)}
	out, err := NewHedge(ctx, nil, log.NewNOPFactory().Logger(), "hedge", opts)
	require.NoError(t, err)
	require.NoError(t, out.(*Hedge).Start())
	return &hedgeFixture{ctx: ctx, hedge: out.(*Hedge), first: first, then: then, mgr: mgr}, obs
}

func TestHedgeObserverSeesCommitToFirstOnce(t *testing.T) {
	f, obs := newObservedHedgeFixture(t, &fakeOutbound{stream: answer("first:")}, &fakeOutbound{stream: answer("then:")})
	ctx := context.WithValue(f.ctx, connLabelKey{}, "conn-1")
	c, err := f.hedge.DialContext(ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Empty(t, obs.committed(), "nothing is reported before First replies")
	require.Equal(t, "first:hello", exchange(t, c, "hello"))
	require.Equal(t, "first:again", exchange(t, c, "again"))
	// A second reader that started before the first reply also commits.
	require.True(t, c.(*hedgeStream).commit(c.(*hedgeStream).releaseLocked))
	require.Equal(t, []string{"conn-1"}, obs.committed(), "one report, with the dial's context")
}

func TestHedgeObserverIgnoresConnectionsOnThen(t *testing.T) {
	f, obs := newObservedHedgeFixture(t, &fakeOutbound{stream: blackhole}, &fakeOutbound{stream: answer("then:")})
	c, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "then:"+tlsRecord("hi"), exchange(t, c, tlsRecord("hi")))

	// The site is now known bad, so this one goes straight to Then.
	c2, err := f.hedge.DialContext(f.ctx, "tcp", hedgeDest)
	require.NoError(t, err)
	defer c2.Close()
	require.Equal(t, "then:x", exchange(t, c2, "x"))
	require.Empty(t, obs.committed())
}

func TestHedgeObserverSeesUDPCommit(t *testing.T) {
	f, obs := newObservedHedgeFixture(t, &fakeOutbound{packet: echoPacket("first:")}, &fakeOutbound{packet: echoPacket("then:")})
	pc, err := f.hedge.ListenPacket(context.WithValue(f.ctx, connLabelKey{}, "quic-1"), hedgeDest)
	require.NoError(t, err)
	defer pc.Close()
	dest := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
	buf := make([]byte, 64)
	require.NoError(t, pc.SetReadDeadline(time.Now().Add(5*time.Second)))
	for _, p := range []string{"initial", "more"} {
		_, err = pc.WriteTo([]byte(p), dest)
		require.NoError(t, err)
		n, _, err := pc.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, "first:"+p, string(buf[:n]))
	}
	require.Equal(t, []string{"quic-1"}, obs.committed())
}
