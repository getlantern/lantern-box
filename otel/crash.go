package otel

import (
	"context"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sagernet/sing-box/log"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

const crashFileName = "crash.log"

// SetupCrashOutput configures debug.SetCrashOutput to write fatal crash
// output (panics, runtime crashes) to a file in dir. On next startup,
// call ReportPreviousCrash to check for and report the crash.
func SetupCrashOutput(dir string) error {
	path := filepath.Join(dir, crashFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	// SetCrashOutput duplicates the fd, so we can close f immediately.
	err = debug.SetCrashOutput(f, debug.CrashOptions{})
	f.Close()
	return err
}

// ReportPreviousCrash checks for a crash log from a previous run. If found,
// it sends the crash as an OTLP log record and deletes the file. This should
// be called early in startup, after the telemetry endpoint is configured.
func ReportPreviousCrash(dir string, opts *Opts) {
	path := filepath.Join(dir, crashFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return // no crash log
	}
	crashLog := strings.TrimSpace(string(data))
	if crashLog == "" {
		return // empty file, no crash
	}

	log.Warn("found crash log from previous run, reporting...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sendCrashLog(ctx, crashLog, opts); err != nil {
		log.Error("failed to report crash log: ", err)
		return
	}

	// Successfully reported — truncate the crash file so it's ready
	// for the next crash. Don't delete it since SetCrashOutput already
	// has the fd.
	os.Truncate(path, 0)
	log.Info("crash log reported and cleared")
}

func sendCrashLog(ctx context.Context, crashLog string, opts *Opts) error {
	exporterOpts := []otlploghttp.Option{
		otlploghttp.WithEndpoint(opts.Endpoint),
		otlploghttp.WithHeaders(opts.Headers),
	}
	if !strings.Contains(opts.Endpoint, ":443") {
		exporterOpts = append(exporterOpts, otlploghttp.WithInsecure())
	}

	exporter, err := otlploghttp.New(ctx, exporterOpts...)
	if err != nil {
		return err
	}
	defer exporter.Shutdown(ctx)

	res := opts.buildResource()
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		sdklog.WithResource(res),
	)
	defer provider.Shutdown(ctx)

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
