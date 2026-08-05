package peerconn

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// accept and close build the two halves of one connection's lifecycle the way
// the inbound call site does, so the tests exercise the same payload shape.
func accept(source, destination string) Event {
	return Event{State: +1, Source: source, Destination: destination}
}

func closed(source string) Event {
	return Event{State: -1, Source: source}
}

func TestNotify_NoListener_NoOp(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })
	SetListener(nil)
	// Just must not panic. Cheap path the standalone CLI exercises on every
	// connection.
	Notify(accept("1.2.3.4:5555", "example.com:443"))
	Notify(closed("1.2.3.4:5555"))
}

func TestSetListener_FiresOnNotify(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var (
		mu     sync.Mutex
		events []Event
	)
	SetListener(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, evt)
	})

	Notify(accept("10.0.0.1:443", "example.com:443"))
	Notify(closed("10.0.0.1:443"))
	Notify(accept("10.0.0.2:443", "spamhaus.example.org:25"))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []Event{
		{State: +1, Source: "10.0.0.1:443", Destination: "example.com:443"},
		{State: -1, Source: "10.0.0.1:443"},
		{State: +1, Source: "10.0.0.2:443", Destination: "spamhaus.example.org:25"},
	}, events)
}

func TestSetListener_LastWriterWins(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var firstHits, secondHits atomic.Int32
	SetListener(func(_ Event) { firstHits.Add(1) })
	Notify(accept("", ""))
	SetListener(func(_ Event) { secondHits.Add(1) })
	Notify(accept("", ""))
	Notify(closed(""))

	assert.Equal(t, int32(1), firstHits.Load(),
		"first listener should see only the pre-replace Notify")
	assert.Equal(t, int32(2), secondHits.Load(),
		"second listener should see the two post-replace Notifies")
}

func TestSetListener_NilUnregisters(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var hits atomic.Int32
	SetListener(func(_ Event) { hits.Add(1) })
	Notify(accept("", ""))
	SetListener(nil)
	Notify(accept("", ""))
	Notify(closed(""))
	assert.Equal(t, int32(1), hits.Load(),
		"after SetListener(nil) further Notifies must not fire")
}

func TestAcquire_NoListener_ReturnsNil(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })
	SetListener(nil)

	assert.Nil(t, Acquire(), "Acquire must report the absence of a listener "+
		"so callers can skip both halves rather than emitting an unpaired accept")
}

// Destination is the abuse-detection signal: present on accept, absent on
// close, because the aggregator pairs a close with the source identity it
// recorded at accept time.
func TestAcquire_AcceptCarriesDestinationCloseOmitsIt(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var (
		mu     sync.Mutex
		events []Event
	)
	SetListener(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, evt)
	})

	notify := Acquire()
	notify(accept("203.0.113.1:5698", "smtp.botnet.example:25"))
	notify(closed("203.0.113.1:5698"))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []Event{
		{State: +1, Source: "203.0.113.1:5698", Destination: "smtp.botnet.example:25"},
		{State: -1, Source: "203.0.113.1:5698"},
	}, events)
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
	SetListener(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		firstStates = append(firstStates, evt.State)
	})

	notify := Acquire()
	notify(accept("10.0.0.1:443", "example.com:443"))
	SetListener(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		replacedStates = append(replacedStates, evt.State)
	})
	notify(closed("10.0.0.1:443"))

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
	SetListener(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, evt.State)
	})

	notify := Acquire()
	notify(accept("", ""))
	SetListener(nil)
	notify(closed(""))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{+1, -1}, states,
		"an accepted connection must still report its close after SetListener(nil)")
}
