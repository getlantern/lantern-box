package otel

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

const crashFileName = "crash.log"

// maxCrashLogSize caps the crash log read to 1 MB to avoid OOM on
// very large goroutine dumps.
const maxCrashLogSize = 1 << 20

// SetupCrashOutput configures debug.SetCrashOutput to write fatal crash
// output (panics, runtime crashes) to a file in dir. On next startup,
// call ReportPreviousCrash to check for and report the crash.
func SetupCrashOutput(dir string) error {
	path := filepath.Join(dir, crashFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	// SetCrashOutput duplicates the fd, so we can close f immediately.
	err = debug.SetCrashOutput(f, debug.CrashOptions{})
	f.Close()
	return err
}

// ReportPreviousCrash checks for a crash log from a previous run. If found,
// it sends the crash as an OTLP log record and truncates the file so it is
// ready for the next crash. This should be called early in startup, after the
// telemetry endpoint is configured.
func ReportPreviousCrash(dir string, attrs ...attribute.KeyValue) {
	path := filepath.Join(dir, crashFileName)
	f, err := os.Open(path)
	if err != nil {
		return // no crash log
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCrashLogSize))
	f.Close()
	if err != nil {
		return
	}
	crashLog := strings.TrimSpace(string(data))
	if crashLog == "" {
		return // empty file, no crash
	}

	log.Warn("found crash log from previous run, reporting...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sendCrashLog(ctx, crashLog, attrs...); err != nil {
		log.Error("failed to report crash log: ", err)
		return
	}

	// Successfully reported — truncate the crash file so it's ready
	// for the next crash. Don't delete it since SetCrashOutput already
	// has the fd.
	if err := os.Truncate(path, 0); err != nil {
		log.Error("failed to truncate crash log: ", err)
		return
	}
	log.Info("crash log reported and cleared")
}

func sendCrashLog(
	ctx context.Context,
	crashLog string,
	attrs ...attribute.KeyValue,
) error {
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return err
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		sdklog.WithResource(buildResource(attrs...)),
	)
	defer func() {
		if err := provider.Shutdown(ctx); err != nil {
			log.Error("failed to shutdown crash log provider: ", err)
		}
	}()

	logger := provider.Logger("lantern-box/crash")

	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(otellog.SeverityFatal)
	record.SetSeverityText("FATAL")
	record.SetBody(otellog.StringValue(crashLog))
	record.AddAttributes(crashAttributes(crashLog)...)

	logger.Emit(ctx, record)
	return nil
}

// crashAttributes returns the log attributes for a crash dump: the crash type
// plus whatever parseCrash could extract, so crashes can be grouped and
// alerted on without searching the dump body (which holds every goroutine,
// so a body search matches frames from goroutines that did not crash).
func crashAttributes(crashLog string) []otellog.KeyValue {
	attrs := []otellog.KeyValue{otellog.String("crash.type", "runtime_panic")}
	c := parseCrash(crashLog)
	for _, kv := range []struct{ key, value string }{
		{"crash.panic", c.panic},
		{"crash.function", c.function},
		{"crash.location", c.location},
		{"crash.signature", c.signature()},
	} {
		if kv.value != "" {
			attrs = append(attrs, otellog.String(kv.key, kv.value))
		}
	}
	return attrs
}

// maxPanicLen caps the panic message, which can embed an arbitrary value.
const maxPanicLen = 512

type crashSummary struct {
	panic    string // first "panic:" or "fatal error:" line
	function string // top non-runtime frame of the crashing goroutine
	location string // file:line of that frame
}

var (
	hexRun   = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	digitRun = regexp.MustCompile(`[0-9]+`)
)

// signature identifies a crash independent of the values involved, so
// "slice bounds out of range [:172] with capacity 128" and "[:257] with
// capacity 256" in the same function group together.
func (c crashSummary) signature() string {
	if c.panic == "" {
		return ""
	}
	sig := digitRun.ReplaceAllString(hexRun.ReplaceAllString(c.panic, "0x?"), "N")
	if c.function != "" {
		sig += " @ " + c.function
	}
	return sig
}

// parseCrash extracts the panic message and crashing frame from a Go crash
// dump. The runtime prints the crashing goroutine first, so the first
// "goroutine N [...]:" block after the panic line is the one that crashed.
func parseCrash(dump string) crashSummary {
	var c crashSummary
	lines := strings.Split(dump, "\n")
	i := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "panic: ") || strings.HasPrefix(line, "fatal error: ") {
			c.panic = truncate(line, maxPanicLen)
			break
		}
	}
	if c.panic == "" {
		return c
	}
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "goroutine ") && strings.HasSuffix(lines[i], "]:") {
			break
		}
	}
	var firstFn, firstLoc string
	for i++; i+1 < len(lines); i += 2 {
		fn, loc := lines[i], lines[i+1]
		if fn == "" || strings.HasPrefix(fn, "created by ") || !strings.HasPrefix(loc, "\t") {
			break
		}
		fn = stripArgs(fn)
		loc, _, _ = strings.Cut(strings.TrimSpace(loc), " +0x")
		if firstFn == "" {
			firstFn, firstLoc = fn, loc
		}
		if !isRuntimeFrame(fn) {
			c.function, c.location = fn, loc
			return c
		}
	}
	// Every frame was in the runtime, e.g. a fatal error raised outside any
	// user code; report the innermost frame rather than nothing.
	c.function, c.location = firstFn, firstLoc
	return c
}

// stripArgs removes the argument list the traceback prints after a frame's
// function name: "pkg.(*T).m(0xc000107140, {...})" becomes "pkg.(*T).m".
func stripArgs(fn string) string {
	if strings.HasSuffix(fn, ")") {
		if i := strings.LastIndex(fn, "("); i > 0 {
			return fn[:i]
		}
	}
	return fn
}

func isRuntimeFrame(fn string) bool {
	return fn == "panic" ||
		strings.HasPrefix(fn, "runtime.") ||
		strings.HasPrefix(fn, "internal/runtime/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cutting mid-rune would leave invalid UTF-8, which OTLP's protobuf
	// encoding rejects.
	return strings.ToValidUTF8(s[:n], "")
}
