package log

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/stretchr/testify/assert"
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
