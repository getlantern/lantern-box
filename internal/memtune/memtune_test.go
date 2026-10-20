package memtune

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/byteformats"
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
	tuner.Resync(nil)

	got := readRuntime()
	assert.Equal(t, int64(2<<30), got.memoryLimit)
	assert.Equal(t, 50, got.gcPercent, "an omitted field keeps the baseline")
}

func TestChangesAndRemovalFollowFile(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")

	tuner := New(path, time.Hour)
	tuner.Resync(nil)
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
	tuner.Resync(nil)

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

// applyConfig stands in for box.New, which applies a config's debug options.
func applyConfig(opts *option.DebugOptions) {
	if opts.GCPercent != nil {
		debug.SetGCPercent(*opts.GCPercent)
	}
	if opts.MemoryLimit.Value() != 0 {
		debug.SetMemoryLimit(int64(float64(opts.MemoryLimit.Value()) / 1.5))
	}
}

// debugOptions parses the options the way a config file does, e.g. "1200MB".
func debugOptions(t *testing.T, limit string, gcPercent int) *option.DebugOptions {
	t.Helper()
	opts := &option.DebugOptions{GCPercent: &gcPercent}
	opts.MemoryLimit = new(byteformats.MemoryBytes)
	require.NoError(t, json.Unmarshal([]byte(`"`+limit+`"`), opts.MemoryLimit))
	return opts
}

func TestBaselineComesFromConfig(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	tuner := New(path, time.Hour)

	cfg := debugOptions(t, "1200MB", 50)
	applyConfig(cfg)
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	tuner.Resync(cfg)
	assert.Equal(t, runtimeValues{2 << 30, 50}, readRuntime())

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{800 << 20, 50}, readRuntime(), "the config's limit, divided by 1.5 as sing-box does")
}

func TestReloadWithoutDebugOptionsDropsOldOverrides(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	tuner := New(path, time.Hour)

	cfg := debugOptions(t, "1200MB", 50)
	applyConfig(cfg)
	writeSettings(t, path, `{"memory_limit_bytes": 3221225472, "gc_percent": 75}`)
	tuner.Resync(cfg)

	// The reloaded config sets no debug options, so building it leaves the
	// file's values in the runtime. They must not become the new baseline.
	tuner.Resync(nil)
	assert.Equal(t, runtimeValues{3 << 30, 75}, readRuntime())

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{1 << 30, 100}, readRuntime(), "back to the startup values")
}

func TestRejectedReloadRestoresSettings(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	tuner := New(path, time.Hour)

	running := debugOptions(t, "1200MB", 50)
	applyConfig(running)
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	tuner.Resync(running)

	// A reload's config check builds a box from the new config and fails.
	applyConfig(debugOptions(t, "600MB", 20))
	tuner.Resync(running)
	assert.Equal(t, runtimeValues{2 << 30, 50}, readRuntime())

	// The unchanged file is still honored by later polls.
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{2 << 30, 50}, readRuntime())
}

func TestBaselineWithGCOff(t *testing.T) {
	withRuntime(t, math.MaxInt64, -1)
	path := filepath.Join(t.TempDir(), "runtime.json")
	writeSettings(t, path, `{"gc_percent": 50}`)

	tuner := New(path, time.Hour)
	tuner.Resync(nil)
	assert.Equal(t, 50, readRuntime().gcPercent)

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{math.MaxInt64, -1}, readRuntime())
}

func TestRunPollsUntilCancelled(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")

	tuner := New(path, 10*time.Millisecond)
	tuner.Resync(nil)
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

func TestReloadKeepsLastValidSettingsOverInvalidFile(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	tuner := New(path, time.Hour)

	running := debugOptions(t, "1200MB", 50)
	applyConfig(running)
	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	tuner.Resync(running)

	writeSettings(t, path, `not json`)
	tuner.checkLocked(false)

	// A reload's config check applies other settings, then the tuner resyncs.
	applyConfig(debugOptions(t, "600MB", 20))
	tuner.Resync(running)
	assert.Equal(t, runtimeValues{2 << 30, 50}, readRuntime())
}

func TestReloadKeepsLastValidSettingsOverUnreadableFile(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tuner := New(path, time.Hour)

	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	tuner.Resync(nil)

	// A directory in place of the file makes ReadFile fail with an error
	// other than "not exist".
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o755))
	debug.SetMemoryLimit(600 << 20)
	tuner.Resync(nil)
	assert.Equal(t, int64(2<<30), readRuntime().memoryLimit)
}

func TestRemovalAfterUnreadableResyncRestoresBaseline(t *testing.T) {
	withRuntime(t, 1<<30, 100)
	path := filepath.Join(t.TempDir(), "runtime.json")
	tuner := New(path, time.Hour)

	writeSettings(t, path, `{"memory_limit_bytes": 2147483648}`)
	tuner.Resync(nil)

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o755))
	tuner.Resync(nil)
	assert.Equal(t, int64(2<<30), readRuntime().memoryLimit)

	require.NoError(t, os.Remove(path))
	tuner.checkLocked(false)
	assert.Equal(t, runtimeValues{1 << 30, 100}, readRuntime(), "removing the path restores the baseline")
}
