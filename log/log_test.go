package log

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogging(t *testing.T) {
	tests := []struct {
		name  string
		level log.Level
	}{
		{"level trace", log.LevelTrace},
		{"level debug", log.LevelDebug},
		{"level info", log.LevelInfo},
		{"level warn", log.LevelWarn},
		{"level error", log.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
				Level: toSLevel(tt.level),
			})
			ctx := context.Background()
			f := &factory{handler: handler}
			sl := &slogLogger{factory: f}
			if tt.level > log.LevelError {
				sl.Log(ctx, tt.level-1, "test", log.FormatLevel(tt.level-1), "message")
				assert.NotEmpty(t, buf.String(), "Expected %s logs", log.FormatLevel(tt.level-1))
			}
			buf.Reset()
			sl.Log(ctx, tt.level, "test", log.FormatLevel(tt.level), "message")
			assert.NotEmpty(t, buf.String(), "Expected log for level %s", log.FormatLevel(tt.level))
			if tt.level < log.LevelTrace {
				buf.Reset()
				sl.Log(ctx, tt.level+1, "test", log.FormatLevel(tt.level+1), "message")
				assert.Empty(t, buf.String(), "Expected no %s logs", log.FormatLevel(tt.level+1))
			}
		})
	}
}

func TestContextID(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	f := &factory{handler: handler}
	sl := &slogLogger{factory: f}

	ctx := log.ContextWithNewID(context.Background())
	sl.Log(ctx, log.LevelInfo, "test message")

	out := buf.String()
	assert.Contains(t, out, "id=", "expected context ID in log output")
	assert.Contains(t, out, "duration=", "expected duration in log output")
}

func TestLevel(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		level log.Level
	}{
		{"trace", "trace", log.LevelTrace},
		{"debug", "debug", log.LevelDebug},
		{"info", "info", log.LevelInfo},
		{"warn", "warn", log.LevelWarn},
		{"error", "error", log.LevelError},
		{"fatal", "fatal", log.LevelFatal},
		{"panic", "panic", log.LevelPanic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
				Level: toSLevel(tt.level),
			})
			f := &factory{handler: handler}
			got := f.Level()
			assert.Equal(t, tt.want, log.FormatLevel(got))
		})
	}
}

func TestSlogLoggerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	f := NewFactory(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { f.Close() })
	parent := f.NewLogger("parent").(SLogger)

	sub, _, err := f.Subscribe()
	require.NoError(t, err)

	parent.WithAttrs(slog.String("group", "g1")).Info("derived")
	out := buf.String()
	assert.Contains(t, out, "group=g1")
	assert.Contains(t, out, "tag=parent")

	select {
	case entry := <-sub:
		assert.Equal(t, "derived", entry.Message)
	case <-time.After(time.Second):
		t.Fatal("derived logger entry not delivered to parent factory's subscribers")
	}

	buf.Reset()
	parent.Info("parent")
	assert.NotContains(t, buf.String(), "group=g1", "WithAttrs must not modify the parent logger")
}

func TestSlogHandlerWithAttrsDoesNotMutate(t *testing.T) {
	h := &slogHandler{}
	derived := h.WithAttrs([]slog.Attr{slog.String("k", "v")})
	assert.Empty(t, h.attrs)
	assert.Contains(t, derived.(*slogHandler).attrs, "k:v")
}

func TestSlogHandlerWithGroupDoesNotAlias(t *testing.T) {
	h := &slogHandler{groups: make([]string, 1, 4)}
	a := h.WithGroup("a").(*slogHandler)
	b := h.WithGroup("b").(*slogHandler)
	assert.Equal(t, []string{"", "a"}, a.groups)
	assert.Equal(t, []string{"", "b"}, b.groups)
	assert.Len(t, h.groups, 1)
}
