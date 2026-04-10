package group

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sboxLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service/pause"
	"github.com/stretchr/testify/assert"
)

func TestURLTestGroup_CloseStopsCheckLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"test": nil}}, // mock or stub
		sboxLog.NewNOPFactory().Logger(),
		[]string{"test"},
		"https://example.com",
		nil,                 // no URL overrides
		10*time.Millisecond, // fast interval
		time.Minute,
		50,
	)

	// Pretend Start/PostStart succeeded
	g.started = true
	g.pauseMgr = &mockPauseManager{}
	g.lastActive.Store(time.Now())

	// Trigger checkLoop
	g.keepAlive()
	assert.Eventually(t, func() bool {
		g.access.Lock()
		defer g.access.Unlock()
		return g.isAlive
	}, time.Second, 200*time.Millisecond)

	// Now stop it
	cancel()
	assert.NoError(t, g.Close())

	// Assert it stops
	assert.Eventually(t, func() bool {
		g.access.Lock()
		defer g.access.Unlock()
		return !g.isAlive
	}, time.Second, 200*time.Millisecond)
}

func TestTestURLForTag_DefaultURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"out1": nil}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"out1"},
		"https://default.example.com",
		nil, // no overrides
		time.Minute, time.Minute, 50,
	)

	assert.Equal(t, "https://default.example.com", g.testURLForTag("out1"))
	assert.Equal(t, "https://default.example.com", g.testURLForTag("unknown"))
}

func TestSetURLOverrides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{
			"out1": nil, "out2": nil, "out3": nil,
		}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"out1", "out2", "out3"},
		"https://default.example.com",
		nil, // start with no overrides
		time.Minute, time.Minute, 50,
	)

	// Initially all tags use the default URL
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out1"))

	// Set overrides
	g.SetURLOverrides(map[string]string{
		"out1": "https://new-override.example.com",
		"out3": "https://out3-override.example.com",
	})
	assert.Equal(t, "https://new-override.example.com", g.testURLForTag("out1"))
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out2"))
	assert.Equal(t, "https://out3-override.example.com", g.testURLForTag("out3"))

	// Replace with different overrides
	g.SetURLOverrides(map[string]string{
		"out2": "https://out2-override.example.com",
	})
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out1"))
	assert.Equal(t, "https://out2-override.example.com", g.testURLForTag("out2"))

	// Clear overrides
	g.SetURLOverrides(nil)
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out1"))
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out2"))
}

func TestSetURLOverrides_DoesNotMutateCallerMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"out1": nil}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"out1"},
		"https://default.example.com",
		nil,
		time.Minute, time.Minute, 50,
	)

	callerMap := map[string]string{"out1": "https://original.example.com"}
	g.SetURLOverrides(callerMap)

	// Mutating the caller's map after SetURLOverrides should not affect the group
	callerMap["out1"] = "https://mutated.example.com"
	assert.Equal(t, "https://original.example.com", g.testURLForTag("out1"))
}

func TestTestURLForTag_WithOverrides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	overrides := map[string]string{
		"out1": "https://override1.example.com",
		"out2": "https://override2.example.com",
	}
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{
			"out1": nil, "out2": nil, "out3": nil,
		}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"out1", "out2", "out3"},
		"https://default.example.com",
		overrides,
		time.Minute, time.Minute, 50,
	)

	// Tags with overrides use their override URL
	assert.Equal(t, "https://override1.example.com", g.testURLForTag("out1"))
	assert.Equal(t, "https://override2.example.com", g.testURLForTag("out2"))
	// Tags without overrides fall back to the default URL
	assert.Equal(t, "https://default.example.com", g.testURLForTag("out3"))
	// Unknown tags also fall back to the default
	assert.Equal(t, "https://default.example.com", g.testURLForTag("unknown"))
}

func TestKeepAlive_ConcurrentCallsNoPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"test": nil}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"test"},
		"https://example.com",
		nil,
		10*time.Millisecond,
		time.Minute,
		50,
	)
	g.started = true
	g.pauseMgr = &mockPauseManager{}
	g.lastActive.Store(time.Now())

	// Call keepAlive rapidly from multiple goroutines. Before the fix, the
	// second call could see isAlive=true with a nil idleTimer and panic.
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			g.keepAlive()
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify invariant: isAlive implies idleTimer != nil
	g.access.Lock()
	if g.isAlive {
		assert.NotNil(t, g.idleTimer, "idleTimer must be non-nil when isAlive is true")
	}
	g.access.Unlock()
}

func TestOnInterfaceUpdated_RestartsDeadCheckLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"test": nil}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"test"},
		"https://example.com",
		nil,
		10*time.Millisecond, // fast interval
		50*time.Millisecond, // short idle timeout
		50,
	)

	g.started = true
	g.pauseMgr = &mockPauseManager{}
	g.lastActive.Store(time.Now())

	// Start the check loop
	g.keepAlive()
	assert.Eventually(t, func() bool {
		g.access.Lock()
		defer g.access.Unlock()
		return g.isAlive
	}, time.Second, 5*time.Millisecond)

	// Wait for the idle timer to kill the check loop
	assert.Eventually(t, func() bool {
		g.access.Lock()
		defer g.access.Unlock()
		return !g.isAlive
	}, time.Second, 10*time.Millisecond, "check loop should have stopped after idle timeout")

	// Simulate a network interface change (e.g. wake from sleep)
	g.onInterfaceUpdated()

	// The check loop should be alive again
	assert.Eventually(t, func() bool {
		g.access.Lock()
		defer g.access.Unlock()
		return g.isAlive
	}, time.Second, 5*time.Millisecond, "onInterfaceUpdated should restart the check loop")
}

func TestOnInterfaceUpdated_NoopWhenClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{"test": nil}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{"test"},
		"https://example.com",
		nil,
		time.Minute, time.Minute, 50,
	)
	g.started = true
	g.pauseMgr = &mockPauseManager{}

	// Close the group
	cancel()
	_ = g.Close()

	// Should not panic or start a check loop
	g.onInterfaceUpdated()

	g.access.Lock()
	alive := g.isAlive
	g.access.Unlock()
	assert.False(t, alive, "onInterfaceUpdated should not start check loop on closed group")
}

func TestOnInterfaceUpdated_NoopWhenNoTags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := newURLTestGroup(
		ctx,
		&mockOutboundManager{outbounds: map[string]adapter.Outbound{}},
		sboxLog.NewNOPFactory().Logger(),
		[]string{}, // no tags
		"https://example.com",
		nil,
		time.Minute, time.Minute, 50,
	)
	g.started = true
	g.pauseMgr = &mockPauseManager{}

	g.onInterfaceUpdated()

	g.access.Lock()
	alive := g.isAlive
	g.access.Unlock()
	assert.False(t, alive, "onInterfaceUpdated should not start check loop with no tags")
}

type mockPauseManager struct{}

func (m *mockPauseManager) DevicePause() {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) DeviceWake() {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) NetworkPause() {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) NetworkWake() {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) IsDevicePaused() bool {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) IsNetworkPaused() bool {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) IsPaused() bool {
	return false
}

func (m *mockPauseManager) WaitActive() {
	panic("not implemented") // TODO: Implement
}

func (m *mockPauseManager) RegisterCallback(callback pause.Callback) *list.Element[pause.Callback] {
	return nil
}

func (m *mockPauseManager) UnregisterCallback(element *list.Element[pause.Callback]) {
}

func TestAppendClientDelay(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		delay    time.Duration
		wantCD   string
		wantPath string
	}{
		{
			name:   "URL with existing query params",
			rawURL: "https://example.com/callback?token=abc",
			delay:  150 * time.Millisecond,
			wantCD: "150",
		},
		{
			name:   "URL without query params",
			rawURL: "https://example.com/callback",
			delay:  42 * time.Millisecond,
			wantCD: "42",
		},
		{
			name:   "URL with multiple params",
			rawURL: "https://example.com/cb?a=1&b=2",
			delay:  300 * time.Millisecond,
			wantCD: "300",
		},
		{
			name:   "sub-millisecond delay rounds to zero",
			rawURL: "https://example.com/cb?x=1",
			delay:  500 * time.Microsecond,
			wantCD: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := appendClientDelay(tt.rawURL, tt.delay)
			u, err := url.Parse(result)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantCD, u.Query().Get("cd"))
			// Original path preserved
			orig, _ := url.Parse(tt.rawURL)
			assert.Equal(t, orig.Path, u.Path)
			assert.Equal(t, orig.Host, u.Host)
		})
	}
}

func TestAppendClientDelay_InvalidURL(t *testing.T) {
	// Invalid URLs are returned unchanged
	bad := "://not-a-url"
	assert.Equal(t, bad, appendClientDelay(bad, 100*time.Millisecond))
}
