package unbounded

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	sblog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	UBClientcore "github.com/getlantern/broflake/clientcore"

	lbAdapter "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/option"
)

func newLoggedTestOutbound(t *testing.T, tag string) *Outbound {
	t.Helper()
	out, err := NewOutbound(context.Background(), nil, sblog.NewNOPFactory().Logger(), tag, option.UnboundedOutboundOptions{})
	require.NoError(t, err)
	return out.(*Outbound)
}

func setConsumerConnected(o *Outbound, connected bool, since, started time.Time) {
	o.path.Store(&helperPathSource{
		connected: func() (bool, time.Time) { return connected, since },
		started:   started,
	})
}

// A helper consumer gives broflake the same no-path grace it reports its
// helper unavailable after, so its flows fail when its status says so. A
// pool consumer keeps broflake's default, which leaves it unguarded.
func TestNewOutbound_HelperPathGrace(t *testing.T) {
	assert.Equal(t, lbAdapter.HelperPathGrace, newTestOutbound(t, context.Background(), "helper-0123456789ab").rtcOpt.ConsumerNoPathGrace)
	assert.Zero(t, newTestOutbound(t, context.Background(), "unbounded-out-pool1").rtcOpt.ConsumerNoPathGrace)
}

func TestHelperPath(t *testing.T) {
	t.Run("pool consumer", func(t *testing.T) {
		o := newTestOutbound(t, context.Background(), "unbounded-out-pool1")
		setConsumerConnected(o, true, time.Now(), time.Now())
		_, ok := o.HelperPath()
		assert.False(t, ok)
	})
	t.Run("not started", func(t *testing.T) {
		_, ok := newTestOutbound(t, context.Background(), "helper-0123456789ab").HelperPath()
		assert.False(t, ok)
	})

	o := newTestOutbound(t, context.Background(), "helper-0123456789ab")
	for _, tc := range []struct {
		name      string
		connected bool
		down      time.Duration
		available bool
	}{
		{"connected", true, time.Minute, true},
		{"dropped within grace", false, lbAdapter.HelperPathGrace / 2, true},
		{"down past grace", false, lbAdapter.HelperPathGrace + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since := time.Now().Add(-tc.down)
			setConsumerConnected(o, tc.connected, since, since.Add(-time.Hour))
			s, ok := o.HelperPath()
			require.True(t, ok)
			assert.Equal(t, tc.connected, s.Connected)
			assert.Equal(t, since, s.Since)
			assert.Equal(t, tc.available, s.Available)
		})
	}
	t.Run("never connected, just started", func(t *testing.T) {
		started := time.Now()
		setConsumerConnected(o, false, time.Time{}, started)
		s, ok := o.HelperPath()
		require.True(t, ok)
		assert.True(t, s.Connecting)
		assert.True(t, s.NeverConnected)
		assert.True(t, s.Available)
		assert.Equal(t, started, s.Since)
	})
	// The helper was offline when the consumer started, as when selecting a
	// friend restarts it: unavailable once broflake fails its dials.
	t.Run("never connected past grace", func(t *testing.T) {
		started := time.Now().Add(-lbAdapter.HelperPathGrace - time.Second)
		setConsumerConnected(o, false, time.Time{}, started)
		s, ok := o.HelperPath()
		require.True(t, ok)
		assert.False(t, s.Connecting)
		assert.True(t, s.NeverConnected)
		assert.False(t, s.Available)
		assert.Equal(t, started, s.Since)
	})
}

// A started helper consumer reports its real engine's path: with no peer ever
// offered, connecting since its own Start. A consumer started later, as a
// restart recreates it, counts from its own Start.
func TestHelperPath_RealEngine(t *testing.T) {
	before := time.Now()
	o := startNoPeerOutbound(t, "helper-0123456789ab")
	s, ok := o.HelperPath()
	require.True(t, ok)
	assert.False(t, s.Connected)
	assert.True(t, s.NeverConnected)
	assert.True(t, s.Connecting)
	assert.True(t, s.Available)
	assert.False(t, s.Since.Before(before), "since is the consumer's start")

	time.Sleep(10 * time.Millisecond)
	restarted := startNoPeerOutbound(t, "helper-0123456789ab")
	s2, ok := restarted.HelperPath()
	require.True(t, ok)
	assert.True(t, s2.Since.After(s.Since), "a restarted consumer starts over")

	_, ok = startNoPeerOutbound(t, "unbounded-out-pool1").HelperPath()
	assert.False(t, ok)
}

func TestHelperUnavailable(t *testing.T) {
	_, ok := helperUnavailable(errors.New("boom"))
	assert.False(t, ok)

	err, ok := helperUnavailable(fmt.Errorf("socks5: %w", UBClientcore.ErrNoConnectedPath))
	require.True(t, ok)
	assert.ErrorIs(t, err, ErrNoHelperConnected)
	assert.ErrorIs(t, err, UBClientcore.ErrNoConnectedPath)

	wrapped := fmt.Errorf("%w: %w", ErrNoHelperConnected, UBClientcore.ErrNoConnectedPath)
	err, ok = helperUnavailable(wrapped)
	require.True(t, ok)
	assert.Same(t, wrapped, err)
}

// A dial broflake fails for want of a path, which can happen when the path
// dies between the slot check and the dial, comes back as the helper being
// unavailable, over TCP, UoT and ListenPacket alike, and at once.
func TestHelperOutbound_NoConnectedPathIsHelperUnavailable(t *testing.T) {
	o := newLoggedTestOutbound(t, "helper-0123456789ab")
	o.consumerSlots = func() []UBClientcore.ConsumerSlot {
		return []UBClientcore.ConsumerSlot{{WorkerIdx: 0, Connected: true}}
	}
	o.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, UBClientcore.ErrNoConnectedPath
	}
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
			err := dial(context.Background())
			assert.ErrorIs(t, err, ErrNoHelperConnected)
			assert.ErrorIs(t, err, UBClientcore.ErrNoConnectedPath)
		})
	}
}
