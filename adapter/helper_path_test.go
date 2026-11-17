package adapter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewHelperPathStatus(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		connected bool
		down      time.Duration
		available bool
	}{
		{"connected", true, time.Hour, true},
		{"just dropped", false, time.Second, true},
		{"within grace", false, HelperPathGrace - time.Millisecond, true},
		{"at grace", false, HelperPathGrace, false},
		{"long down", false, time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since := now.Add(-tc.down)
			s := NewHelperPathStatus(tc.connected, since, now)
			assert.Equal(t, tc.connected, s.Connected)
			assert.Equal(t, since, s.Since)
			assert.Equal(t, tc.available, s.Available)
			assert.False(t, s.Connecting)
		})
	}
}

// A path that has never connected is connecting, not lost, however long it
// has been looking.
func TestNewHelperPathStatus_NeverConnected(t *testing.T) {
	s := NewHelperPathStatus(false, time.Time{}, time.Now())
	assert.False(t, s.Connected)
	assert.True(t, s.Connecting)
	assert.True(t, s.Since.IsZero())
	assert.True(t, s.Available)
}
