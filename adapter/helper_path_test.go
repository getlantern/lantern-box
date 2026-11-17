package adapter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// HelperPathGrace mirrors broflake's default no-path grace, one room
// attempt's worst case.
func TestHelperPathGrace(t *testing.T) {
	assert.Equal(t, 20*time.Second, HelperPathGrace)
}

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
			s := NewHelperPathStatus(tc.connected, since, now.Add(-time.Hour), now)
			assert.Equal(t, tc.connected, s.Connected)
			assert.Equal(t, since, s.Since)
			assert.Equal(t, tc.available, s.Available)
			assert.False(t, s.Connecting)
			assert.False(t, s.NeverConnected)
		})
	}
}

// A consumer that has never connected is connecting for HelperPathGrace
// after it starts, and unavailable after that, since its start: a helper
// that is offline when the consumer (re)starts is the usual way to have none.
func TestNewHelperPathStatus_NeverConnected(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		running    time.Duration
		connecting bool
	}{
		{"just started", 0, true},
		{"within grace", HelperPathGrace - time.Millisecond, true},
		{"at grace", HelperPathGrace, false},
		{"long after", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := now.Add(-tc.running)
			s := NewHelperPathStatus(false, time.Time{}, started, now)
			assert.False(t, s.Connected)
			assert.True(t, s.NeverConnected)
			assert.Equal(t, tc.connecting, s.Connecting)
			assert.Equal(t, tc.connecting, s.Available)
			assert.Equal(t, started, s.Since)
		})
	}
}
