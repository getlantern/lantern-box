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
