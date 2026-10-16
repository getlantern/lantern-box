package memtune

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests change process-wide GC settings, so they don't run in parallel and
// each one restores what it found.
func withRuntime(t *testing.T, limit int64, gcPercent int) {
	t.Helper()
	prevLimit := debug.SetMemoryLimit(limit)
	prevGC := debug.SetGCPercent(gcPercent)
	t.Cleanup(func() {
		debug.SetMemoryLimit(prevLimit)
		debug.SetGCPercent(prevGC)
	})
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestResyncAppliesFileOverBaseline(t *testing.T) {
	withRuntime(t, 1<<30, 50)
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)

	tuner := New(path, time.Hour)
	tuner.Resync()

	got := readRuntime()
	assert.Equal(t, int64(2<<30), got.memoryLimit)
	assert.Equal(t, 50, got.gcPercent, "an omitted field keeps the baseline")
}

func TestChangesAndRemovalFollowFile(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")

	tuner := New(path, time.Hour)
	tuner.Resync()
	assert.Equal(t, runtimeValues{1 << 30, 100}, readRuntime(), "no file leaves the runtime alone")

	writeSettings(t, path, `{"memory_limit_bytes": 3221225472, "gc_percent": 50}`)
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{3 << 30, 50}, readRuntime())

	writeSettings(t, path, `{"memory_limit_bytes": 4294967296}`)
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{4 << 30, 100}, readRuntime(), "a dropped field returns to the baseline")

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{1 << 30, 100}, readRuntime(), "removing the file restores the baseline")
}

func TestInvalidFileKeepsCurrentSettings(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)

	tuner := New(path, time.Hour)
	tuner.Resync()

	for _, bad := range []string{
		`{"memory_limit_bytes": 1024}`,
		`{"gc_percent": 0}`,
		`{"gc_percent": -1}`,
		`not json`,
		``,
	} {
		writeSettings(t, path, bad)
		tuner.checkLocked(false)
		assert.Equal(t, runtimeValues{2 << 30, 100}, readRuntime(), "content %q", bad)
	}
}

func TestResyncReappliesAfterReload(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648, "gc_percent": 50}`)

	tuner := New(path, time.Hour)
	tuner.Resync()

	// A SIGHUP reload rebuilds the box, which re-applies the config's options.
	debug.SetMemoryLimit(800 << 20)
	debug.SetGCPercent(75)
	tuner.Resync()

	assert.Equal(t, runtimeValues{2 << 30, 50}, readRuntime())

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{800 << 20, 75}, readRuntime(), "the baseline is the reloaded config's")
}

func TestBaselineWithGCOff(t *testing.T) {
	withRuntime(t, math.MaxInt64, -1)
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeSettings(t, path, `{"gc_percent": 50}`)

	tuner := New(path, time.Hour)
	tuner.Resync()
	assert.Equal(t, 50, readRuntime().gcPercent)

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{math.MaxInt64, -1}, readRuntime())
}

func TestRunPollsUntilCancelled(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")

	tuner := New(path, 10*time.Millisecond)
	tuner.Resync()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tuner.Run(ctx)
		close(done)
	}()

	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	assert.Eventually(t, func() bool {
		return debug.SetMemoryLimit(-1) == 2<<30
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
