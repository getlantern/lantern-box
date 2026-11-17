package outline

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/sagernet/sing/common/logger"
	"golang.getoutline.org/sdk/x/smart"
)

// fileStrategyCache is a smart.StrategyResultCache kept in a JSON file, so the
// strategy a search found survives a restart. Failures to read or write the
// file are logged and otherwise ignored: the cache only saves a search.
type fileStrategyCache struct {
	path   string
	logger logger.ContextLogger

	mu      sync.Mutex
	entries map[string][]byte
}

var _ smart.StrategyResultCache = (*fileStrategyCache)(nil)

func newFileStrategyCache(path string, logger logger.ContextLogger) *fileStrategyCache {
	c := &fileStrategyCache{path: path, logger: logger, entries: map[string][]byte{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		c.warn("outline: read strategy cache: ", err)
	default:
		if err := json.Unmarshal(data, &c.entries); err != nil {
			c.warn("outline: discard unreadable strategy cache: ", err)
			c.entries = map[string][]byte{}
		}
	}
	return c
}

func (c *fileStrategyCache) warn(args ...any) {
	if c.logger != nil {
		c.logger.Warn(args...)
	}
}

func (c *fileStrategyCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.entries[key]
	return value, ok
}

// Put stores value under key, or removes key when value is nil, and rewrites
// the file through a rename so a crash never leaves it half written.
func (c *fileStrategyCache) Put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		delete(c.entries, key)
	} else {
		c.entries[key] = value
	}
	if err := c.save(); err != nil {
		c.warn("outline: write strategy cache: ", err)
	}
}

func (c *fileStrategyCache) save() error {
	data, err := json.Marshal(c.entries)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
