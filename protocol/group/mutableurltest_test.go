package group

import (
	"context"
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
		nil, // no URL overrides
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
