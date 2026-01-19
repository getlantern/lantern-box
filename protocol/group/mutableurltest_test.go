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
	time.Sleep(500 * time.Millisecond)
	g.access.Lock()
	assert.True(t, g.isAlive)
	g.access.Unlock()

	// Now stop it
	cancel()
	assert.NoError(t, g.Close())

	// Assert it stops
	time.Sleep(500 * time.Millisecond)
	g.access.Lock()
	assert.False(t, g.isAlive)
	g.access.Unlock()
}
