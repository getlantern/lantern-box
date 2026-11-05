package samizdatreverse

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	v4 = netip.MustParseAddrPort("192.0.2.1:443")
	v6 = netip.MustParseAddrPort("[2001:db8::1]:443")
)

// fakeConn is a net.Conn that remembers which address it was dialed to and
// whether it was closed.
type fakeConn struct {
	net.Conn
	addr   netip.AddrPort
	mu     sync.Mutex
	closed bool
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// dialPlan says how a fake dial to an address behaves: it waits for after,
// then fails with err or succeeds. A negative after blocks until ctx ends.
type dialPlan struct {
	after time.Duration
	err   error
}

type fakeDialer struct {
	plans map[netip.AddrPort]dialPlan
	start time.Time

	mu      sync.Mutex
	started map[netip.AddrPort]time.Duration
	conns   []*fakeConn
}

func newFakeDialer(plans map[netip.AddrPort]dialPlan) *fakeDialer {
	return &fakeDialer{plans: plans, start: time.Now(), started: make(map[netip.AddrPort]time.Duration)}
}

func (d *fakeDialer) dial(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	d.mu.Lock()
	d.started[addr] = time.Since(d.start)
	d.mu.Unlock()
	plan := d.plans[addr]
	if plan.after < 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-time.After(plan.after):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if plan.err != nil {
		return nil, plan.err
	}
	c := &fakeConn{addr: addr}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

func (d *fakeDialer) startedAt(addr netip.AddrPort) (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	at, ok := d.started[addr]
	return at, ok
}

func TestEyeballsIPv6WinsWithinDelay(t *testing.T) {
	d := newFakeDialer(map[netip.AddrPort]dialPlan{
		v6: {after: 50 * time.Millisecond},
		v4: {after: 0},
	})
	conn, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v4, v6}, 250*time.Millisecond, d.dial)
	require.NoError(t, err)
	assert.Equal(t, v6, conn.(*fakeConn).addr)
	_, started := d.startedAt(v4)
	assert.False(t, started, "IPv4 started although IPv6 connected within the delay")
}

func TestEyeballsIPv4WinsWhenIPv6Hangs(t *testing.T) {
	d := newFakeDialer(map[netip.AddrPort]dialPlan{
		v6: {after: -1},
		v4: {after: 0},
	})
	conn, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v6, v4}, 100*time.Millisecond, d.dial)
	require.NoError(t, err)
	assert.Equal(t, v4, conn.(*fakeConn).addr)
	at, ok := d.startedAt(v4)
	require.True(t, ok)
	assert.GreaterOrEqual(t, at, 100*time.Millisecond, "IPv4 started before the delay")
}

func TestEyeballsIPv6FailureStartsIPv4AtOnce(t *testing.T) {
	d := newFakeDialer(map[netip.AddrPort]dialPlan{
		v6: {err: errors.New("network unreachable")},
		v4: {after: 0},
	})
	conn, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v4, v6}, 5*time.Second, d.dial)
	require.NoError(t, err)
	assert.Equal(t, v4, conn.(*fakeConn).addr)
	at, ok := d.startedAt(v4)
	require.True(t, ok)
	assert.Less(t, at, time.Second, "IPv4 waited for the delay after IPv6 failed")
}

func TestEyeballsClosesLoser(t *testing.T) {
	// IPv6 is slow, so IPv4 starts and wins; IPv6 then connects, and that
	// conn must be closed rather than leaked. Its dial ignores ctx so that it
	// does connect.
	var late *fakeConn
	lateDone := make(chan struct{})
	dial := func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		if addr == v6 {
			defer close(lateDone)
			time.Sleep(150 * time.Millisecond)
			late = &fakeConn{addr: addr}
			return late, nil
		}
		return &fakeConn{addr: addr}, nil
	}
	conn, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v6, v4}, 50*time.Millisecond, dial)
	require.NoError(t, err)
	assert.Equal(t, v4, conn.(*fakeConn).addr)
	<-lateDone
	require.Eventually(t, late.isClosed, time.Second, 10*time.Millisecond)
	assert.False(t, conn.(*fakeConn).isClosed())
}

func TestEyeballsSingleFamily(t *testing.T) {
	for _, addr := range []netip.AddrPort{v4, v6} {
		d := newFakeDialer(map[netip.AddrPort]dialPlan{addr: {after: 0}})
		conn, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{addr}, 250*time.Millisecond, d.dial)
		require.NoError(t, err)
		assert.Equal(t, addr, conn.(*fakeConn).addr)
	}

	d := newFakeDialer(map[netip.AddrPort]dialPlan{v4: {err: errors.New("refused")}})
	_, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v4}, 250*time.Millisecond, d.dial)
	assert.Error(t, err)
}

func TestEyeballsAllFail(t *testing.T) {
	d := newFakeDialer(map[netip.AddrPort]dialPlan{
		v6: {err: errors.New("unreachable")},
		v4: {err: errors.New("refused")},
	})
	start := time.Now()
	_, err := dialHappyEyeballs(context.Background(), []netip.AddrPort{v4, v6}, 5*time.Second, d.dial)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestEyeballsCancelReturnsPromptly(t *testing.T) {
	// The dials ignore ctx entirely, so only dialHappyEyeballs itself can
	// return early.
	block := make(chan struct{})
	defer close(block)
	dial := func(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
		<-block
		return nil, errors.New("unblocked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := dialHappyEyeballs(ctx, []netip.AddrPort{v6, v4}, 20*time.Millisecond, dial)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
}

func TestEyeballsNoAddrs(t *testing.T) {
	_, err := dialHappyEyeballs(context.Background(), nil, time.Second, newFakeDialer(nil).dial)
	assert.Error(t, err)
}
