package outline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStrategyCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strategies.json")

	c := newFileStrategyCache(path, nil)
	if _, ok := c.Get("winner"); ok {
		t.Fatal("fresh cache has an entry")
	}
	c.Put("winner", []byte(`{"tls":"split:2"}`))
	c.Put("other", []byte("x"))

	reloaded := newFileStrategyCache(path, nil)
	if got, ok := reloaded.Get("winner"); !ok || string(got) != `{"tls":"split:2"}` {
		t.Fatalf("after restart Get = %q, %v", got, ok)
	}

	reloaded.Put("winner", nil)
	if _, ok := newFileStrategyCache(path, nil).Get("winner"); ok {
		t.Fatal("Put(nil) did not remove the entry from the file")
	}
	if got, ok := newFileStrategyCache(path, nil).Get("other"); !ok || string(got) != "x" {
		t.Fatalf("removing one key lost another: %q, %v", got, ok)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := newFileStrategyCache(path, nil)
	if _, ok := corrupt.Get("other"); ok {
		t.Fatal("corrupt file produced entries")
	}
	corrupt.Put("winner", []byte("y"))
	if got, ok := newFileStrategyCache(path, nil).Get("winner"); !ok || string(got) != "y" {
		t.Fatalf("cache did not recover from a corrupt file: %q, %v", got, ok)
	}

	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	null := newFileStrategyCache(path, nil)
	null.Put("winner", []byte("z")) // must not panic on a nil map
	if got, ok := newFileStrategyCache(path, nil).Get("winner"); !ok || string(got) != "z" {
		t.Fatalf("cache did not recover from a null file: %q, %v", got, ok)
	}
}

func TestFileStrategyCacheCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "data", "strategies.json")
	newFileStrategyCache(path, nil).Put("winner", []byte("w"))
	if got, ok := newFileStrategyCache(path, nil).Get("winner"); !ok || string(got) != "w" {
		t.Fatalf("cache in a missing directory did not persist: %q, %v", got, ok)
	}
}

func TestStrategyCacheSharedPerPath(t *testing.T) {
	dir := t.TempDir()
	a := strategyCacheFor(filepath.Join(dir, "s.json"), nil)
	b := strategyCacheFor(filepath.Join(dir, ".", "s.json"), nil)
	if a != b {
		t.Fatal("outbounds naming the same file must share one cache")
	}
	a.Put("from-a", []byte("1"))
	b.Put("from-b", []byte("2"))
	reloaded := newFileStrategyCache(filepath.Join(dir, "s.json"), nil)
	if _, ok := reloaded.Get("from-a"); !ok {
		t.Fatal("one outbound's save dropped the other's entry")
	}
}
