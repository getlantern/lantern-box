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
		"second listener should see the two post-replace Notifys")
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
		"after SetListener(nil) further Notifys must not fire")
}
