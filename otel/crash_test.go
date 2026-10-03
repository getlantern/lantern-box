package otel

import (
	"os"
	"strings"
	"testing"

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
				panic:    "panic: runtime error: slice bounds out of range [:172] with capacity 128",
				function: "github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
				location: "github.com/sagernet/sing-vmess@v0.2.8-0.20250909125414-3aed155119a1/vless/vision.go:259",
			},
			signature: "panic: runtime error: slice bounds out of range [:N] with capacity N" +
				" @ github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS",
		},
		{
			name: "banditprobe format panic",
			dump: readTestdata(t, "crash-banditprobe.txt"),
			want: crashSummary{
				panic:    "panic: unknown value",
				function: "github.com/sagernet/sing/common/format.ToString",
				location: "github.com/sagernet/sing@v0.8.13/common/format/fmt.go:61",
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
				panic:    "panic: runtime error: invalid memory address or nil pointer dereference",
				function: "github.com/getlantern/lantern-box/protocol/foo.(*conn).Read",
				location: "github.com/getlantern/lantern-box/protocol/foo/conn.go:42",
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
				panic:    "fatal error: concurrent map writes",
				function: "github.com/getlantern/lantern-box/tracker/datacap.(*Tracker).add",
				location: "github.com/getlantern/lantern-box/tracker/datacap/tracker.go:88",
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
				panic:    "fatal error: out of memory",
				function: "runtime.throw",
				location: "/usr/local/go/src/runtime/panic.go:1101",
			},
			signature: "fatal error: out of memory @ runtime.throw",
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
	a := crashSummary{panic: "panic: runtime error: slice bounds out of range [:172] with capacity 128", function: "f"}
	b := crashSummary{panic: "panic: runtime error: slice bounds out of range [:257] with capacity 256", function: "f"}
	assert.Equal(t, a.signature(), b.signature())
}

func TestCrashAttributes(t *testing.T) {
	attrs := crashAttributes(readTestdata(t, "crash-vless-vision.txt"))
	got := map[string]string{}
	for _, kv := range attrs {
		require.Equal(t, otellog.KindString, kv.Value.Kind())
		got[kv.Key] = kv.Value.AsString()
	}
	assert.Equal(t, "runtime_panic", got["crash.type"])
	assert.Equal(t, "github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS", got["crash.function"])
	assert.Contains(t, got["crash.signature"], "@ github.com/sagernet/sing-vmess/vless.(*VisionConn).filterTLS")

	assert.Equal(t,
		[]otellog.KeyValue{otellog.String("crash.type", "runtime_panic")},
		crashAttributes("not a crash"),
		"unparseable dumps still report the crash type")
}

func TestCrashPanicTruncatedToValidUTF8(t *testing.T) {
	dump := "panic: " + strings.Repeat("é", maxPanicLen) + "\n\ngoroutine 1 [running]:\n"
	p := parseCrash(dump).panic
	assert.LessOrEqual(t, len(p), maxPanicLen)
	assert.True(t, strings.HasPrefix(p, "panic: é"))
	assert.Equal(t, p, strings.ToValidUTF8(p, ""))
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}
