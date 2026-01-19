package group

import (
	"context"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	sboxLog "github.com/sagernet/sing-box/log"
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
