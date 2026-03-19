package otel

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	record.AddAttributes(
		otellog.String("crash.type", "runtime_panic"),
	)

	logger.Emit(ctx, record)
	return nil
}
