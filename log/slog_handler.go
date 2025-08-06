package log

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sagernet/sing-box/log"
)

// slogHandler is a Handler that implements the slog.Handler interface
// and writes log records to a log.ContextLogger.
type slogHandler struct {
	logger   log.ContextLogger
	minLevel slog.Level
	opts     slog.HandlerOptions
	attrs    string
	groups   []string
}

// NewLogHandler returns a new slog.Handler that writes log records to the given log.ContextLogger
func NewLogHandler(logger log.ContextLogger) slog.Handler {
	return &slogHandler{logger: logger}
}

// Enabled reports whether the handler handles records at the given level.
func (h *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	minLevel := slog.LevelDebug
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return level >= minLevel
}

// Handle handles the Record.
func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.Enabled(ctx, record.Level) {
		return nil
	}

	messageBuilder := new(strings.Builder)
	messageBuilder.WriteString(record.Message)
	messageBuilder.WriteString(" ")
	for i, group := range h.groups {
		messageBuilder.WriteString(group)
		if i < len(h.groups)-1 {
			messageBuilder.WriteString(".")
		}
	}
	record.Attrs(func(attr slog.Attr) bool {
		messageBuilder.WriteString(attr.Key)
		messageBuilder.WriteString("=")
		messageBuilder.WriteString(attr.Value.String())
		messageBuilder.WriteString(" ")
		return true
	})

	messageBuilder.WriteString(h.attrs)
	message := messageBuilder.String()

	switch record.Level {
	case slog.LevelDebug:
		h.logger.DebugContext(ctx, message)
	case slog.LevelInfo:
		h.logger.InfoContext(ctx, message)
	case slog.LevelWarn:
		h.logger.WarnContext(ctx, message)
	case slog.LevelError:
		h.logger.ErrorContext(ctx, message)
	default:
		return fmt.Errorf("unsupported log level: %v", record.Level)
	}
	return nil
}

// WithAttrs returns a new Handler whose attributes consist of
// both the receiver's attributes and the arguments.
func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = fmt.Sprintf("%s %s", h.attrs, parseAttrs(attrs))
	return h
}

func parseAttrs(attrs []slog.Attr) string {
	attrsBuilder := new(strings.Builder)
	for _, attr := range attrs {
		attrsBuilder.WriteString(attr.Key)
		attrsBuilder.WriteString(":")
		switch attr.Value.Kind() {
		case slog.KindString, slog.KindAny:
			attrsBuilder.WriteString(attr.Value.String())
		case slog.KindInt64:
			fmt.Fprintf(attrsBuilder, "%d", attr.Value.Int64())
		case slog.KindUint64:
			fmt.Fprintf(attrsBuilder, "%d", attr.Value.Uint64())
		case slog.KindFloat64:
			fmt.Fprintf(attrsBuilder, "%f", attr.Value.Float64())
		case slog.KindBool:
			fmt.Fprintf(attrsBuilder, "%t", attr.Value.Bool())
		case slog.KindTime:
			attrsBuilder.WriteString(attr.Value.Time().String())
		case slog.KindDuration:
			attrsBuilder.WriteString(attr.Value.Duration().String())
		case slog.KindLogValuer:
			attrsBuilder.WriteString(attr.Value.LogValuer().LogValue().String())
		case slog.KindGroup:
			attrsBuilder.WriteString("{")
			attrsBuilder.WriteString(parseAttrs(attr.Value.Group()))
			attrsBuilder.WriteString("}")
		}
		attrsBuilder.WriteString(" ")
	}
	return attrsBuilder.String()
}

// WithGroup returns a new Handler with the given group appended to
// the receiver's existing groups.
func (h *slogHandler) WithGroup(name string) slog.Handler {
	newGroups := append(h.groups, name)
	return &slogHandler{
		logger:   h.logger,
		minLevel: h.minLevel,
		opts:     h.opts,
		attrs:    h.attrs,
		groups:   newGroups,
	}
}
