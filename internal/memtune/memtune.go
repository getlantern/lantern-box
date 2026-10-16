// Package memtune applies Go runtime memory settings from a file that the host
// can rewrite while the process runs, so a container's GC soft limit can follow
// the host's memory budget without a restart.
//
// The file is JSON:
//
//	{"memory_limit_bytes": 4294967296, "gc_percent": 50}
//
// memory_limit_bytes is passed to debug.SetMemoryLimit as is (unlike sing-box's
// experimental.debug.memory_limit, which is divided by 1.5). Either field may be
// omitted to leave that setting at its baseline: the running config's
// experimental.debug value, else the process's startup value. The baseline is
// restored when the file is removed or a field is dropped.
package memtune

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
)

const (
	// MinMemoryLimit rejects budgets too small for a proxy to run under; a
	// limit below the live heap makes the GC run continuously.
	MinMemoryLimit = 64 << 20
	minGCPercent   = 10
	maxGCPercent   = 1000

	DefaultInterval = 10 * time.Second
)

// Settings is the file's schema.
type Settings struct {
	MemoryLimitBytes *int64 `json:"memory_limit_bytes,omitempty"`
	GCPercent        *int   `json:"gc_percent,omitempty"`
}

func (s Settings) validate() error {
	if s.MemoryLimitBytes != nil && *s.MemoryLimitBytes < MinMemoryLimit {
		return fmt.Errorf("memory_limit_bytes %d is below the %d minimum", *s.MemoryLimitBytes, MinMemoryLimit)
	}
	if s.GCPercent != nil && (*s.GCPercent < minGCPercent || *s.GCPercent > maxGCPercent) {
		return fmt.Errorf("gc_percent %d is outside %d-%d", *s.GCPercent, minGCPercent, maxGCPercent)
	}
	return nil
}

type runtimeValues struct {
	memoryLimit int64
	gcPercent   int
}

// Tuner polls one settings file and applies it to the Go runtime.
type Tuner struct {
	path     string
	interval time.Duration
	startup  runtimeValues

	mu       sync.Mutex
	baseline runtimeValues
	content  []byte   // last file content seen; nil when the file is absent
	settings Settings // last valid file settings; empty when the file is absent
	applied  runtimeValues
}

// New returns a Tuner for path. It records the runtime's current settings as
// the startup values, so call it before any config is applied. Then call
// Resync once the service is created, and Run to poll.
func New(path string, interval time.Duration) *Tuner {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Tuner{path: path, interval: interval, startup: readRuntime()}
}

// Resync sets the baseline from the running config's debug options and
// applies the file on top. Call it whenever anything may have built a box
// (service creation, and a reload's config check, whether it passed or not),
// since building a box applies that config's debug options to the runtime.
func (t *Tuner) Resync(debugOptions *option.DebugOptions) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.baseline = t.startup
	if debugOptions != nil {
		if debugOptions.GCPercent != nil {
			t.baseline.gcPercent = *debugOptions.GCPercent
		}
		// Mirrors sing-box's applyDebugOptions, which divides by 1.5.
		if debugOptions.MemoryLimit.Value() != 0 {
			t.baseline.memoryLimit = int64(float64(debugOptions.MemoryLimit.Value()) / 1.5)
		}
	}
	// The runtime may hold anything now; mark it unknown so the next apply
	// sets both values.
	t.applied = runtimeValues{memoryLimit: -1, gcPercent: math.MinInt}
	t.content = nil
	t.checkLocked(true)
}

// Run polls the file until ctx is done.
func (t *Tuner) Run(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.mu.Lock()
			t.checkLocked(false)
			t.mu.Unlock()
		}
	}
}

func (t *Tuner) checkLocked(force bool) {
	content, err := os.ReadFile(t.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		content = nil
	case err != nil:
		// Keep the last valid settings; on a Resync they still need applying.
		log.Warn("memtune: reading ", t.path, ": ", err)
		t.applyLocked()
		return
	}
	// bytes.Equal treats nil and empty as equal; an empty file must still be
	// told apart from a missing one.
	if !force && bytes.Equal(content, t.content) && (content == nil) == (t.content == nil) {
		return
	}
	t.content = content

	if content == nil {
		t.settings = Settings{}
	} else {
		var s Settings
		if err := json.Unmarshal(content, &s); err != nil {
			log.Warn("memtune: ignoring ", t.path, ", keeping the last valid settings: ", err)
		} else if err := s.validate(); err != nil {
			log.Warn("memtune: ignoring ", t.path, ", keeping the last valid settings: ", err)
		} else {
			t.settings = s
		}
	}
	t.applyLocked()
}

// applyLocked sets the runtime to the baseline with the last valid file
// settings on top.
func (t *Tuner) applyLocked() {
	want := t.baseline
	if t.settings.MemoryLimitBytes != nil {
		want.memoryLimit = *t.settings.MemoryLimitBytes
	}
	if t.settings.GCPercent != nil {
		want.gcPercent = *t.settings.GCPercent
	}
	if want == t.applied {
		return
	}
	if want.memoryLimit != t.applied.memoryLimit {
		debug.SetMemoryLimit(want.memoryLimit)
	}
	if want.gcPercent != t.applied.gcPercent {
		debug.SetGCPercent(want.gcPercent)
	}
	log.Info("memtune: applied memory_limit=", want.memoryLimit, " gc_percent=", want.gcPercent)
	t.applied = want
}

func readRuntime() runtimeValues {
	samples := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(samples)
	return runtimeValues{
		memoryLimit: debug.SetMemoryLimit(-1), // a negative input only reads the limit
		// GOGC=off (-1) reads back as MaxUint64; the int64 conversion restores -1.
		gcPercent: int(int64(samples[0].Value.Uint64())),
	}
}
