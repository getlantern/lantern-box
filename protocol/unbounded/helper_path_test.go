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

func setConsumerConnected(o *Outbound, connected bool, since time.Time) {
	f := func() (bool, time.Time) { return connected, since }
	o.consumerConnected.Store(&f)
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
		setConsumerConnected(o, true, time.Now())
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
			setConsumerConnected(o, tc.connected, since)
			s, ok := o.HelperPath()
			require.True(t, ok)
			assert.Equal(t, tc.connected, s.Connected)
			assert.Equal(t, since, s.Since)
			assert.Equal(t, tc.available, s.Available)
		})
	}
	t.Run("never connected", func(t *testing.T) {
		setConsumerConnected(o, false, time.Time{})
		s, ok := o.HelperPath()
		require.True(t, ok)
		assert.True(t, s.Connecting)
		assert.True(t, s.Available)
	})
}

// A started helper consumer reports its real engine's path: with no peer ever
// offered, not connected, and not lost. Broflake reports a zero since until a
// slot first connects (a version before that reported the engine's start).
func TestHelperPath_RealEngine(t *testing.T) {
	o := startNoPeerOutbound(t, "helper-0123456789ab")
	s, ok := o.HelperPath()
	require.True(t, ok)
	assert.False(t, s.Connected)
	if !s.Since.IsZero() {
		assert.WithinDuration(t, time.Now(), s.Since, 5*time.Second)
	}
	assert.True(t, s.Available)

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
