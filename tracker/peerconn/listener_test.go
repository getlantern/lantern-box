package peerconn

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNotify_NoListener_NoOp(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })
	SetListener(nil)
	// Just must not panic. Cheap path the standalone CLI exercises on every
	// connection.
	Notify(+1, "1.2.3.4:5555")
	Notify(-1, "1.2.3.4:5555")
}

func TestSetListener_FiresOnNotify(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	type call struct {
		state  int
		source string
	}
	var (
		mu    sync.Mutex
		calls []call
	)
	SetListener(func(state int, source string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{state, source})
	})

	Notify(+1, "10.0.0.1:443")
	Notify(-1, "10.0.0.1:443")
	Notify(+1, "10.0.0.2:443")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []call{
		{+1, "10.0.0.1:443"},
		{-1, "10.0.0.1:443"},
		{+1, "10.0.0.2:443"},
	}, calls)
}

func TestSetListener_LastWriterWins(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var firstHits, secondHits atomic.Int32
	SetListener(func(_ int, _ string) { firstHits.Add(1) })
	Notify(+1, "")
	SetListener(func(_ int, _ string) { secondHits.Add(1) })
	Notify(+1, "")
	Notify(-1, "")

	assert.Equal(t, int32(1), firstHits.Load(),
		"first listener should see only the pre-replace Notify")
	assert.Equal(t, int32(2), secondHits.Load(),
		"second listener should see the two post-replace Notifies")
}

func TestSetListener_NilUnregisters(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var hits atomic.Int32
	SetListener(func(_ int, _ string) { hits.Add(1) })
	Notify(+1, "")
	SetListener(nil)
	Notify(+1, "")
	Notify(-1, "")
	assert.Equal(t, int32(1), hits.Load(),
		"after SetListener(nil) further Notifies must not fire")
}

func TestAcquire_NoListener_ReturnsNil(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })
	SetListener(nil)

	assert.Nil(t, Acquire(), "Acquire must report the absence of a listener "+
		"so callers can skip both halves rather than emitting an unpaired accept")
}

// The close half of a connection must reach whichever listener accepted it.
// Acquiring once is what guarantees that; reading the registry per half would
// hand the close to whatever listener happened to be registered by then,
// leaving the accepting listener with a connection that never closes.
func TestAcquire_PairSurvivesListenerReplacement(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var (
		mu             sync.Mutex
		firstStates    []int
		replacedStates []int
	)
	SetListener(func(state int, _ string) {
		mu.Lock()
		defer mu.Unlock()
		firstStates = append(firstStates, state)
	})

	notify := Acquire()
	notify(+1, "10.0.0.1:443")
	SetListener(func(state int, _ string) {
		mu.Lock()
		defer mu.Unlock()
		replacedStates = append(replacedStates, state)
	})
	notify(-1, "10.0.0.1:443")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{+1, -1}, firstStates,
		"both halves must reach the listener that accepted the connection")
	assert.Empty(t, replacedStates,
		"the replacement listener must not see a close for a connection it never accepted")
}

// Documents the deliberate consequence of pairing: unregistering mid-connection
// no longer suppresses the close half, because dropping it would strand the
// accept the consumer has already recorded. Consumers wanting a hard stop gate
// inside their own callback.
func TestAcquire_PairCompletesAfterUnregister(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var (
		mu     sync.Mutex
		states []int
	)
	SetListener(func(state int, _ string) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, state)
	})

	notify := Acquire()
	notify(+1, "")
	SetListener(nil)
	notify(-1, "")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{+1, -1}, states,
		"an accepted connection must still report its close after SetListener(nil)")
}
