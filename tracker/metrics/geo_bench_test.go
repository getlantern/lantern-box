package metrics

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/getlantern/geo"
	"github.com/stretchr/testify/require"
)

// findGeoTestMMDB locates the test mmdb bundled with the geo module in the
// module cache by asking `go list` for the module's directory.
func findGeoTestMMDB() string {
	out, err := exec.Command("go", "list", "-m", "-json", "github.com/getlantern/geo").Output()
	if err != nil {
		return ""
	}
	var info struct{ Dir string }
	if err := json.Unmarshal(out, &info); err != nil || info.Dir == "" {
		return ""
	}
	return filepath.Join(info.Dir, "GeoIP2-City-Test.mmdb")
}

func loadBenchLookup(b *testing.B) geo.CountryLookup {
	b.Helper()
	path := findGeoTestMMDB()
	if path == "" {
		b.Skip("could not locate geo test mmdb")
	}
	if _, err := os.Stat(path); err != nil {
		b.Skipf("geo test mmdb not found: %v", err)
	}
	l, err := geo.FromFile(path)
	require.NoError(b, err)
	return l
}

// BenchmarkGeoLookupDirect measures the raw in-memory mmdb lookup with no
// timeout goroutine overhead.
func BenchmarkGeoLookupDirect(b *testing.B) {
	l := loadBenchLookup(b)
	ip := net.ParseIP("81.2.69.142") // present in the test db
	b.ResetTimer()
	for b.Loop() {
		l.CountryCode(ip)
	}
}

// BenchmarkGeoLookupMiss measures the lookup for an IP absent from the test db.
func BenchmarkGeoLookupMiss(b *testing.B) {
	l := loadBenchLookup(b)
	ip := net.ParseIP("8.8.8.8") // not in the small test db
	b.ResetTimer()
	for b.Loop() {
		l.CountryCode(ip)
	}
}

// BenchmarkGeoLookupWithTimeout measures the overhead of the goroutine +
// timer path used when lookupTimeout > 0.
func BenchmarkGeoLookupWithTimeout(b *testing.B) {
	l := loadBenchLookup(b)
	ip := net.ParseIP("81.2.69.142")
	b.ResetTimer()
	for b.Loop() {
		ch := make(chan string, 1)
		go func() { ch <- l.CountryCode(ip) }()
		select {
		case <-ch:
		case <-time.After(time.Millisecond):
		}
	}
}
