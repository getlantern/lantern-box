package clientcontext

import (
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
)

// An excluded inbound is never read for client info, even under a wildcard, and the exclusion
// survives SetBounds and MatchBounds.
func TestMatchBoundsExcludeInbound(t *testing.T) {
	bounds := MatchBounds{Inbound: []string{""}, Outbound: []string{""}, ExcludeInbound: []string{"unbounded-exit"}}
	m := NewManager(bounds, log.NewNOPFactory().Logger())
	assert.False(t, m.match("unbounded-exit", "direct"))
	assert.True(t, m.match("samizdat-in", "direct"))

	got := m.MatchBounds()
	assert.Equal(t, bounds, got)
	got.ExcludeInbound[0] = "other"
	assert.Equal(t, []string{"unbounded-exit"}, m.MatchBounds().ExcludeInbound, "MatchBounds returns a copy")

	m.SetBounds(MatchBounds{Inbound: []string{""}, Outbound: []string{""}})
	assert.True(t, m.match("unbounded-exit", "direct"))
	m.SetBounds(bounds)
	assert.False(t, m.match("unbounded-exit", "direct"))
}
