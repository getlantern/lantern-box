package portforward

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setLocalIPProbeAddr(t *testing.T, addr string) {
	t.Helper()
	prev := localIPProbeAddr
	localIPProbeAddr = addr
	t.Cleanup(func() { localIPProbeAddr = prev })
}

func TestForwarder_MapPort_LocalIPControlDialsTheProbe(t *testing.T) {
	// Loopback, so the dial needs no route to the internet and its local
	// address is known.
	setLocalIPProbeAddr(t, "127.0.0.1:53")
	var calls atomic.Int32
	var gotNetwork, gotAddress string
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	f.SetLocalIPControl(func(network, address string, _ syscall.RawConn) error {
		calls.Add(1)
		gotNetwork, gotAddress = network, address
		return nil
	})

	m, err := f.MapPort(context.Background(), UDP, 30001, "test")
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "the local IP dial runs through the control")
	assert.Equal(t, "udp4", gotNetwork)
	assert.Equal(t, "127.0.0.1:53", gotAddress)
	assert.Equal(t, "127.0.0.1", m.InternalIP)
	assert.Equal(t, "127.0.0.1", c.lastAdd.internalClient, "the gateway is asked to forward to the controlled dial's address")
}

func TestForwarder_MapPort_LocalIPControlFailureDoesNotScan(t *testing.T) {
	if _, err := localIPByInterfaceScan(); err != nil {
		t.Skipf("no private ipv4 interface, so a scan fallback couldn't be told apart: %v", err)
	}
	setLocalIPProbeAddr(t, "127.0.0.1:53")
	c := &fakeIGD{}
	f := newTestForwarder(t, c)
	refused := errors.New("refused by control")
	f.SetLocalIPControl(func(string, string, syscall.RawConn) error { return refused })

	_, err := f.MapPort(context.Background(), UDP, 30001, "test")
	require.Error(t, err, "a failed controlled dial must not fall back to scanning interfaces")
	assert.ErrorIs(t, err, ErrNoLocalIP)
	assert.ErrorIs(t, err, refused)
	assert.Zero(t, c.addCalls.Load(), "nothing is mapped")
}

func TestForwarder_MapPort_NoLocalIPControlKeepsScanFallback(t *testing.T) {
	scanned, err := localIPByInterfaceScan()
	if err != nil {
		t.Skipf("no private ipv4 interface available: %v", err)
	}
	// An address the dial can't use, so only the scan can answer.
	setLocalIPProbeAddr(t, "no-port")
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	m, err := f.MapPort(context.Background(), UDP, 30001, "test")
	require.NoError(t, err)
	assert.Equal(t, scanned, m.InternalIP, "without a control, a failed dial still falls back to the scan")
}

func TestForwarder_MapPort_NoLocalIPControlDialsUnbound(t *testing.T) {
	setLocalIPProbeAddr(t, "127.0.0.1:53")
	c := &fakeIGD{}
	f := newTestForwarder(t, c)

	m, err := f.MapPort(context.Background(), UDP, 30001, "test")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", m.InternalIP, "without a control, the plain dial answers first")
}
