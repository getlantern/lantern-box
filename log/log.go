package log

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/observable"
)

const (
	LevelTrace = slog.LevelDebug - 4 // no trace level in slog, use debug-4.
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
	LevelFatal = slog.LevelError + 4 // no fatal level in slog, use error+4.
	LevelPanic = slog.LevelError + 8 // no panic level in slog, use error+8.
)

type Factory interface {
	log.ObservableFactory
	SlogHandler() slog.Handler
}

type factory struct {
	handler slog.Handler

	subscriber *observable.Subscriber[log.Entry]
	observer   *observable.Observer[log.Entry]
}

// NewFactory wraps a [slog.Handler] into a [Factory] implementation and is meant to be used with sing-box.
func NewFactory(
	handler slog.Handler,
) Factory {
	factory := &factory{
		handler:    handler,
		subscriber: observable.NewSubscriber[log.Entry](128),
	}
	factory.observer = observable.NewObserver[log.Entry](factory.subscriber, 64)
	return factory
}

// Start implements the [Factory] interface and is a no-op in this implementation.
func (f *factory) Start() error {
	return nil
}

// Close implements the [Factory] interface and closes the subscriber. Close does not close the logger.
// It is the responsibility of the caller to close the logger if needed.
func (f *factory) Close() error {
	return common.Close(
		f.subscriber,
	)
}

// Level returns the current logging level of the factory.
func (f *factory) Level() log.Level {
	for i := log.LevelTrace; i >= log.LevelPanic; i-- {
		if f.handler.Enabled(context.Background(), toSLevel(i)) {
			return i
		}
	}
	return log.LevelTrace
}

// SetLevel implements the [Factory] interface. [slog.Handler] does not support dynamic level changes,
// so this method is a no-op.
func (f *factory) SetLevel(level log.Level) {}

// Logger implements the [Factory] interface and returns a [log.ContextLogger] with an empty tag.
func (f *factory) Logger() log.ContextLogger {
	return &slogLogger{factory: f}
}

// NewLogger implements the [Factory] interface and returns a [log.ContextLogger] with the provided
// tag.
func (f *factory) NewLogger(tag string) log.ContextLogger {
	nf := f.clone()
	return &slogLogger{factory: nf, tag: tag}
}

func (f *factory) clone() *factory {
	nf := *f
	return &nf
}

// Subscribe implements the [log.ObservableFactory] interface and returns a subscription to log entries.
func (f *factory) Subscribe() (subscription observable.Subscription[log.Entry], done <-chan struct{}, err error) {
	return f.observer.Subscribe()
}

// UnSubscribe implements the [log.ObservableFactory] interface and unsubscribes from log entries.
func (f *factory) UnSubscribe(sub observable.Subscription[log.Entry]) {
	f.observer.UnSubscribe(sub)
}

// SlogHandler returns the underlying [slog.Handler] used by this factory.
func (f *factory) SlogHandler() slog.Handler {
	return f.handler
}

// SLogger is an interface that writes logs to a [slog.Handler] and is compatible with sing-box
type SLogger interface {
	Factory
	log.ContextLogger
}

var _ SLogger = (*slogLogger)(nil)

type slogLogger struct {
	*factory
	tag string
}

// Log accepts a [sing-box/log.Level] to satisfy the [log.ContextLogger] interface. It is converted
// to [slog.Level] internally.
func (l *slogLogger) Log(ctx context.Context, level log.Level, args ...any) string {
	if len(args) == 0 {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return l.log(ctx, toSLevel(level), args)
}

func (l *slogLogger) log(ctx context.Context, level slog.Level, args []any) string {
	if !l.handler.Enabled(ctx, level) {
		return ""
	}

	for i, arg := range args {
		if s, ok := arg.(string); ok {
			args[i] = strings.TrimSpace(s)
		}
	}

	format := strings.Join(slices.Repeat([]string{"%v"}, len(args)), " ")
	message := fmt.Sprintf(format, args...)
	args = []any{}
	if l.tag != "" {
		args = append(args, slog.String("tag", l.tag))
	}
	if ctx != nil {
		if id, hasId := log.IDFromContext(ctx); hasId {
			args = append(args, slog.Duration("duration", time.Since(id.CreatedAt)))
		}
	}

	var pcs [1]uintptr
	// skip [runtime.Callers, this function, the caller of this function]
	runtime.Callers(3, pcs[:])
	pc := pcs[0]

	r := slog.NewRecord(time.Now(), level, message, pc)
	r.Add(args...)
	_ = l.SlogHandler().Handle(ctx, r)

	if l.subscriber != nil {
		l.subscriber.Emit(log.Entry{
			Level:   log.Level(level),
			Message: message,
		})
	}
	return message
}

func (l *slogLogger) Trace(args ...any) {
	l.log(context.Background(), LevelTrace, args)
}

func (l *slogLogger) Debug(args ...any) {
	l.log(context.Background(), LevelDebug, args)
}

func (l *slogLogger) Info(args ...any) {
	l.log(context.Background(), LevelInfo, args)
}

func (l *slogLogger) Warn(args ...any) {
	l.log(context.Background(), LevelWarn, args)
}

func (l *slogLogger) Error(args ...any) {
	l.log(context.Background(), LevelError, args)
}

func (l *slogLogger) Fatal(args ...any) {
	l.log(context.Background(), LevelFatal, args)
	os.Exit(1)
}

func (l *slogLogger) Panic(args ...any) {
	message := l.log(context.Background(), LevelPanic, args)
	panic(message)
}

func (l *slogLogger) TraceContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelTrace, args)
}

func (l *slogLogger) DebugContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelDebug, args)
}

func (l *slogLogger) InfoContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelInfo, args)
}

func (l *slogLogger) WarnContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelWarn, args)
}

func (l *slogLogger) ErrorContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelError, args)
}

func (l *slogLogger) FatalContext(ctx context.Context, args ...any) {
	l.log(ctx, LevelFatal, args)
	os.Exit(1)
}

func (l *slogLogger) PanicContext(ctx context.Context, args ...any) {
	message := l.log(ctx, LevelPanic, args)
	panic(message)
}

// SBLevelToString converts a [log.Level] to its string representation.
func SBLevelToString(l log.Level) string {
	return strings.ToUpper(log.FormatLevel(l))
}

// toSLevel converts a [log.Level] to a [slog.Level]. This is necessary because slog and sing-box
// use different level representations.
func toSLevel(lvl log.Level) slog.Level {
	switch lvl {
	case log.LevelTrace:
		return LevelTrace
	case log.LevelDebug:
		return slog.LevelDebug
	case log.LevelInfo:
		return slog.LevelInfo
	case log.LevelWarn:
		return slog.LevelWarn
	case log.LevelError:
		return slog.LevelError
	case log.LevelFatal:
		return LevelFatal
	case log.LevelPanic:
		return LevelPanic
	default:
		return slog.LevelDebug // Default to debug if the level is unknown.
	}
}
