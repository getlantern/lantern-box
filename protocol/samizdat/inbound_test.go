package samizdat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/route/rule"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"

	"github.com/getlantern/lantern-box/tracker/peerconn"
)

// mockRouter implements adapter.ConnectionRouterEx with controllable callback behavior.
type mockRouter struct {
	onRoute func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc)
}

func (m *mockRouter) RouteConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext) error {
	return nil
}

func (m *mockRouter) RoutePacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	return nil
}

func (m *mockRouter) RouteConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	if m.onRoute != nil {
		m.onRoute(ctx, conn, metadata, onClose)
	}
}

func (m *mockRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
}

// closeTrackingConn wraps a net.Conn and records when Close is called.
type closeTrackingConn struct {
	net.Conn
	closed *atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// dummyConn returns one end of a net.Pipe, closing the other end.
func dummyConn(t *testing.T) net.Conn {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return c1
}

func TestHandleConnection_BlocksUntilCallbackFires(t *testing.T) {
	callbackFired := make(chan struct{})
	router := &mockRouter{
		onRoute: func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
			// Simulate async routing: fire callback after a delay in a goroutine
			go func() {
				time.Sleep(100 * time.Millisecond)
				onClose(nil)
				close(callbackFired)
			}()
		},
	}

	ib := &Inbound{
		logger: log.NewNOPFactory().Logger(),
		router: router,
	}

	done := make(chan struct{})
	go func() {
		ib.handleConnection(context.Background(), dummyConn(t), "1.2.3.4:80")
		close(done)
	}()

	// handleConnection should NOT return before the callback fires
	select {
	case <-done:
		t.Fatal("handleConnection returned before routing callback fired")
	case <-time.After(50 * time.Millisecond):
		// Good — still blocking
	}

	// Wait for the callback and verify handleConnection returns
	<-callbackFired
	select {
	case <-done:
		// Good — returned after callback
	case <-time.After(time.Second):
		t.Fatal("handleConnection did not return after routing callback fired")
	}
}

func TestHandleConnection_ContextCancelClosesConnAndWaitsForCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var connClosed atomic.Bool
	callbackFired := make(chan struct{})

	// Use a pipe so we can observe when Close is called.
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	wrapped := &closeTrackingConn{Conn: c1, closed: &connClosed}

	router := &mockRouter{
		onRoute: func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
			// Simulate sing-box's fire-and-forget: spawn goroutines, return.
			// The onClose fires after the goroutines detect the context is canceled.
			go func() {
				// Wait for the context to be canceled, then fire the callback.
				<-ctx.Done()
				time.Sleep(50 * time.Millisecond) // small delay to simulate real goroutine cleanup
				onClose(nil)
				close(callbackFired)
			}()
		},
	}

	ib := &Inbound{
		logger: log.NewNOPFactory().Logger(),
		router: router,
	}

	done := make(chan struct{})
	go func() {
		ib.handleConnection(ctx, wrapped, "1.2.3.4:80")
		close(done)
	}()

	// Should be blocking
	select {
	case <-done:
		t.Fatal("handleConnection returned before context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	// After cancel, handleConnection should close the conn, wait for
	// the callback, then return.
	select {
	case <-done:
		// Good — returned after callback fired
	case <-time.After(2 * time.Second):
		t.Fatal("handleConnection did not return after context cancellation + callback")
	}

	// Verify the conn was closed (which is how the goroutines are unblocked)
	assert.True(t, connClosed.Load(), "conn should have been closed after context cancel")
}

func TestHandleConnection_ContextCancelTimesOutIfCallbackNeverFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	router := &mockRouter{
		onRoute: func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
			// Never call onClose — simulates a completely hung route
		},
	}

	ib := &Inbound{
		logger:          log.NewNOPFactory().Logger(),
		router:          router,
		shutdownTimeout: 100 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		ib.handleConnection(ctx, dummyConn(t), "1.2.3.4:80")
		close(done)
	}()

	cancel()

	// Should return within the short shutdownTimeout + small buffer
	select {
	case <-done:
		// Good — timed out and returned
	case <-time.After(time.Second):
		t.Fatal("handleConnection did not return after context cancellation + timeout")
	}
}

func TestHandleConnection_SetsMetadata(t *testing.T) {
	var captured adapter.InboundContext
	router := &mockRouter{
		onRoute: func(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
			captured = metadata
			onClose(nil)
		},
	}

	ib := &Inbound{
		Adapter: inbound.NewAdapter("samizdat", "samizdat-in"),
		logger:  log.NewNOPFactory().Logger(),
		router:  router,
	}

	ib.handleConnection(context.Background(), dummyConn(t), "93.184.216.34:443")

	assert.Equal(t, "samizdat-in", captured.Inbound)
	assert.Equal(t, "samizdat", captured.InboundType)
	assert.Equal(t, "93.184.216.34", captured.Destination.Addr.String())
	assert.Equal(t, uint16(443), captured.Destination.Port)
}

// The close event carries the destination only when a reject rule refused it,
// so a consumer can tally refusals without confusing them with dial failures
// or connections that simply ended.
func TestHandleConnection_CloseEventMarksRejections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		routeErr error
		rejected bool
	}{
		{"routed", nil, false},
		{"dial failure", errors.New("dial tcp: connection refused"), false},
		{"reject rule", &rule.RejectedError{Cause: errors.New("reset")}, true},
		{"wrapped reject", fmt.Errorf("route: %w", &rule.RejectedError{}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				events []peerconn.Event
			)
			peerconn.SetListener(func(evt peerconn.Event) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, evt)
			})
			t.Cleanup(func() { peerconn.SetListener(nil) })

			ib := &Inbound{
				logger: log.NewNOPFactory().Logger(),
				router: &mockRouter{onRoute: func(_ context.Context, _ net.Conn, _ adapter.InboundContext, onClose N.CloseHandlerFunc) {
					onClose(tc.routeErr)
				}},
			}
			ib.handleConnection(context.Background(), dummyConn(t), "blocked.example:443")

			mu.Lock()
			defer mu.Unlock()
			if !assert.Len(t, events, 2) {
				return
			}
			assert.Equal(t, +1, events[0].State)
			assert.Equal(t, "blocked.example:443", events[0].Destination)
			assert.False(t, events[0].Rejected)
			closeEvt := events[1]
			assert.Equal(t, -1, closeEvt.State)
			assert.Equal(t, tc.rejected, closeEvt.Rejected)
			if tc.rejected {
				assert.Equal(t, "blocked.example:443", closeEvt.Destination)
			} else {
				assert.Empty(t, closeEvt.Destination)
			}
		})
	}
}
