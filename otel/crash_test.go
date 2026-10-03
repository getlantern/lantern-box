package otel

import (
	"os"
	"strings"
	"testing"

	"github.com/getlantern/semconv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
)

func TestParseCrash(t *testing.T) {
	tests := []struct {
		name      string
		dump      string
		want      crashSummary
		signature string
	}{
		{
			name: "vless vision slice bounds",
			dump: readTestdata(t, "crash-vless-vision.txt"),
			want: crashSummary{
				kind:     "panic",
				message:  "runtime error: slice bounds out of range [:172] with capacity 128",
				function: "github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
				file:     "github.com/sagernet/sing-vmess@v0.2.8-0.20250909125414-3aed155119a1/vless/vision.go",
				line:     259,
			},
			signature: "panic: runtime error: slice bounds out of range [:N] with capacity N" +
				" @ github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
		},
		{
			name: "banditprobe format panic",
			dump: readTestdata(t, "crash-banditprobe.txt"),
			want: crashSummary{
				kind:     "panic",
				message:  "unknown value",
				function: "github.com/sagernet/sing/common/format.ToString",
				file:     "github.com/sagernet/sing@v0.8.13/common/format/fmt.go",
				line:     61,
			},
			signature: "panic: unknown value @ github.com/sagernet/sing/common/format.ToString",
		},
		{
			name: "nil dereference skips runtime frames and signal line",
			dump: `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x4a1b2c]

goroutine 77 [running]:
runtime.panicmem(...)
	/usr/local/go/src/runtime/panic.go:262 +0x2e
github.com/getlantern/lantern-box/protocol/foo.(*conn).Read(0x0, {0xc000123000, 0x10, 0x10})
	github.com/getlantern/lantern-box/protocol/foo/conn.go:42 +0x1c
created by github.com/getlantern/lantern-box/protocol/foo.New in goroutine 12
	github.com/getlantern/lantern-box/protocol/foo/foo.go:10 +0x55
`,
			want: crashSummary{
				kind:     "panic",
				message:  "runtime error: invalid memory address or nil pointer dereference",
				function: "github.com/getlantern/lantern-box/protocol/foo.(*conn).Read",
				file:     "github.com/getlantern/lantern-box/protocol/foo/conn.go",
				line:     42,
			},
			signature: "panic: runtime error: invalid memory address or nil pointer dereference" +
				" @ github.com/getlantern/lantern-box/protocol/foo.(*conn).Read",
		},
		{
			name: "fatal error",
			dump: `fatal error: concurrent map writes

goroutine 9 [running]:
internal/runtime/maps.fatal({0x1d2e3f?, 0x0?})
	/usr/local/go/src/runtime/panic.go:1058 +0x18
github.com/getlantern/lantern-box/tracker/datacap.(*Tracker).add(0xc0001a2000, {0xc00001c0a0, 0x8})
	github.com/getlantern/lantern-box/tracker/datacap/tracker.go:88 +0x4c
`,
			want: crashSummary{
				kind:     "fatal error",
				message:  "concurrent map writes",
				function: "github.com/getlantern/lantern-box/tracker/datacap.(*Tracker).add",
				file:     "github.com/getlantern/lantern-box/tracker/datacap/tracker.go",
				line:     88,
			},
			signature: "fatal error: concurrent map writes" +
				" @ github.com/getlantern/lantern-box/tracker/datacap.(*Tracker).add",
		},
		{
			name: "only runtime frames falls back to the innermost",
			dump: `fatal error: out of memory

goroutine 1 [running]:
runtime.throw({0x1b2c3d?, 0x0?})
	/usr/local/go/src/runtime/panic.go:1101 +0x48
runtime.mallocgc(0x40000000, 0x0, 0x0)
	/usr/local/go/src/runtime/malloc.go:1050 +0x5a
`,
			want: crashSummary{
				kind:     "fatal error",
				message:  "out of memory",
				function: "runtime.throw",
				file:     "/usr/local/go/src/runtime/panic.go",
				line:     1101,
			},
			signature: "fatal error: out of memory @ runtime.throw",
		},
		{
			name: "empty panic message",
			dump: "panic: \n\ngoroutine 1 [running]:\nmain.main()\n\t/app/main.go:5 +0x25\n",
			want: crashSummary{
				kind:     "panic",
				function: "main.main",
				file:     "/app/main.go",
				line:     5,
			},
			signature: "panic @ main.main",
		},
		{
			name: "not a crash dump",
			dump: "some unrelated output\nwith no panic line\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCrash(tt.dump)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.signature, got.signature())
		})
	}
}

func TestCrashSignatureIgnoresValues(t *testing.T) {
	a := crashSummary{kind: "panic", message: "runtime error: slice bounds out of range [:172] with capacity 128", function: "f"}
	b := crashSummary{kind: "panic", message: "runtime error: slice bounds out of range [:257] with capacity 256", function: "f"}
	assert.Equal(t, a.signature(), b.signature())

	addr := crashSummary{kind: "panic", message: "bad pointer 0x4a1b2c in span 12", function: "f"}
	assert.Equal(t, "panic: bad pointer 0x? in span N @ f", addr.signature())
}

func TestCrashAttributes(t *testing.T) {
	attrs := crashAttributes(readTestdata(t, "crash-vless-vision.txt"))
	got := map[string]otellog.Value{}
	for _, kv := range attrs {
		got[kv.Key] = kv.Value
	}
	assert.Equal(t, "runtime_panic", got[string(semconv.CrashTypeKey)].AsString())
	assert.Equal(t, "runtime error: slice bounds out of range [:172] with capacity 128",
		got[string(semconv.ExceptionMessageKey)].AsString())
	assert.Equal(t, "github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
		got[string(semconv.CodeFunctionNameKey)].AsString())
	assert.Equal(t, "github.com/sagernet/sing-vmess@v0.2.8-0.20250909125414-3aed155119a1/vless/vision.go",
		got[string(semconv.CodeFilePathKey)].AsString())
	assert.Equal(t, otellog.KindInt64, got[string(semconv.CodeLineNumberKey)].Kind())
	assert.Equal(t, int64(259), got[string(semconv.CodeLineNumberKey)].AsInt64())
	assert.Equal(t,
		"panic: runtime error: slice bounds out of range [:N] with capacity N @ github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
		got[string(semconv.CrashSignatureKey)].AsString())

	assert.Equal(t,
		[]otellog.KeyValue{otellog.String(string(semconv.CrashTypeKey), "runtime_panic")},
		crashAttributes("not a crash"),
		"unparseable dumps still report the crash type")
}

func TestCrashMessageTruncatedToValidUTF8(t *testing.T) {
	// An odd byte budget against 2-byte runes forces a mid-rune cut.
	dump := "panic: x" + strings.Repeat("é", maxMessageLen) + "\n\ngoroutine 1 [running]:\n"
	msg := parseCrash(dump).message
	assert.LessOrEqual(t, len(msg), maxMessageLen)
	assert.True(t, strings.HasPrefix(msg, "xé"))
	assert.Equal(t, msg, strings.ToValidUTF8(msg, ""))
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}
