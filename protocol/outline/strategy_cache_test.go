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
}
