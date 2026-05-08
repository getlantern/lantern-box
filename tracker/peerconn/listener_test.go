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
	NotifyAccept("1.2.3.4:5555", "example.com:443")
	NotifyClose("1.2.3.4:5555")
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

	NotifyAccept("10.0.0.1:443", "example.com:443")
	NotifyClose("10.0.0.1:443")
	NotifyAccept("10.0.0.2:443", "spamhaus.example.org:25")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []Event{
		{State: +1, Source: "10.0.0.1:443", Destination: "example.com:443"},
		{State: -1, Source: "10.0.0.1:443"},
		{State: +1, Source: "10.0.0.2:443", Destination: "spamhaus.example.org:25"},
	}, events)
}

func TestNotifyAccept_CarriesDestination(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var got Event
	SetListener(func(evt Event) { got = evt })
	NotifyAccept("203.0.113.1:5698", "smtp.botnet.example:25")

	assert.Equal(t, +1, got.State)
	assert.Equal(t, "203.0.113.1:5698", got.Source)
	assert.Equal(t, "smtp.botnet.example:25", got.Destination,
		"destination is the abuse-detection load-bearing field — must round-trip")
}

func TestNotifyClose_OmitsDestination(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var got Event
	SetListener(func(evt Event) { got = evt })
	NotifyClose("203.0.113.1:5698")

	assert.Equal(t, -1, got.State)
	assert.Equal(t, "203.0.113.1:5698", got.Source)
	assert.Empty(t, got.Destination,
		"close events must NOT carry destination — abuse aggregator pairs by source identity at +1 time")
}

func TestSetListener_LastWriterWins(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var firstHits, secondHits atomic.Int32
	SetListener(func(_ Event) { firstHits.Add(1) })
	NotifyAccept("", "")
	SetListener(func(_ Event) { secondHits.Add(1) })
	NotifyAccept("", "")
	NotifyClose("")

	assert.Equal(t, int32(1), firstHits.Load(),
		"first listener should see only the pre-replace Notify")
	assert.Equal(t, int32(2), secondHits.Load(),
		"second listener should see the two post-replace Notifys")
}

func TestSetListener_NilUnregisters(t *testing.T) {
	t.Cleanup(func() { SetListener(nil) })

	var hits atomic.Int32
	SetListener(func(_ Event) { hits.Add(1) })
	NotifyAccept("", "")
	SetListener(nil)
	NotifyAccept("", "")
	NotifyClose("")
	assert.Equal(t, int32(1), hits.Load(),
		"after SetListener(nil) further Notifys must not fire")
}
