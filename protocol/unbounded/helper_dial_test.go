package unbounded

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sblog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	UBClientcore "github.com/getlantern/broflake/clientcore"

	"github.com/getlantern/lantern-box/option"
)

// startNoPeerOutbound starts a real broflake consumer against a discovery
// server that never offers a peer, so none of its slots ever connects.
func startNoPeerOutbound(t *testing.T, tag string) *Outbound {
	t.Helper()
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(discovery.Close)

	out, err := NewOutbound(context.Background(), nil, sblog.NewNOPFactory().Logger(), tag, option.UnboundedOutboundOptions{
		DiscoverySrv:                  discovery.URL,
		InsecureDoNotVerifyClientCert: true,
	})
	require.NoError(t, err)
	o := out.(*Outbound)
	require.NoError(t, o.Start(adapter.StartStatePostStart))
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// A helper consumer has no pool to fall back to, so with no helper connected
// its dials must fail at once rather than wait for a peer until the caller's
// context ends: route dials have no deadline, and a url-test group that
// picked it would hang instead of trying its other members.
func TestHelperOutbound_NoConnectedSlotFailsFast(t *testing.T) {
	o := startNoPeerOutbound(t, "helper-0123456789ab")
	dest := M.ParseSocksaddr("example.com:443")

	for name, dial := range map[string]func(context.Context) error{
		"tcp": func(ctx context.Context) error { _, err := o.DialContext(ctx, N.NetworkTCP, dest); return err },
		"udp": func(ctx context.Context) error { _, err := o.DialContext(ctx, N.NetworkUDP, dest); return err },
		"listen packet": func(ctx context.Context) error {
			_, err := o.ListenPacket(ctx, dest)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := time.Now()
			err := dial(ctx)
			assert.ErrorIs(t, err, ErrNoHelperConnected)
			assert.Less(t, time.Since(started), 500*time.Millisecond, "dial waited for a peer")
		})
	}
}

// A pool consumer keeps waiting for a peer, as before.
func TestPoolOutbound_NoConnectedSlotWaits(t *testing.T) {
	o := startNoPeerOutbound(t, "unbounded-out-pool1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := o.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddr("example.com:443"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoHelperConnected)
	assert.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond, "a pool dial waits for a peer")
}

// One connected slot is enough. No slots at all, as a non-consumer engine
// reports, counts as none connected.
func TestCheckHelperConnected_AnyConnectedSlot(t *testing.T) {
	o := newTestOutbound(t, context.Background(), "helper-0123456789ab")
	o.consumerSlots = func() []UBClientcore.ConsumerSlot {
		return []UBClientcore.ConsumerSlot{{WorkerIdx: 0, Connected: false}, {WorkerIdx: 1, Connected: true}}
	}
	assert.NoError(t, o.checkHelperConnected())
	o.consumerSlots = func() []UBClientcore.ConsumerSlot { return []UBClientcore.ConsumerSlot{{WorkerIdx: 0}} }
	assert.ErrorIs(t, o.checkHelperConnected(), ErrNoHelperConnected)
	o.consumerSlots = func() []UBClientcore.ConsumerSlot { return nil }
	assert.ErrorIs(t, o.checkHelperConnected(), ErrNoHelperConnected)
}
