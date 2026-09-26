// Package cache is the Collector's local manifest cache, cache.json in the
// state directory (collector.md §3.2). It records the last state the Hub
// acknowledged for each Raw record, so unchanged files are skipped by stat.
package cache

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fileVersion = 1

// Entry is the Hub's acked state of one record, plus the file's stat at the
// time of that ack.
type Entry struct {
	Length   int64     `json:"length"`
	Sha256   string    `json:"sha256"`
	SrcSize  int64     `json:"src_size"`
	SrcMtime time.Time `json:"src_mtime"`
}

// Unchanged reports whether fi has the size and mtime recorded at the ack,
// so the record can be skipped without reading it.
func (e Entry) Unchanged(fi fs.FileInfo) bool {
	return e.SrcSize == fi.Size() && e.SrcMtime.Equal(fi.ModTime())
}

// Key is the cache key for a record: source + NUL + Record key.
func Key(source, recordKey string) string { return source + "\x00" + recordKey }

type file struct {
	Version                 int              `json:"version"`
	HubURL                  string           `json:"hub_url"`
	Records                 map[string]Entry `json:"records"`
	OpencodeLastTimeUpdated int64            `json:"opencode_last_time_updated,omitempty"`
	UnclaimedSeen           []string         `json:"unclaimed_seen,omitempty"`
}

// Cache is safe for concurrent use.
type Cache struct {
	path  string
	mu    sync.Mutex
	f     file
	dirty bool
}

// Load reads the cache at path. A missing or unreadable file, or one written
// for a different hub_url, gives an empty cache that the next reconcile
// rebuilds.
func Load(path, hubURL string, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}
	c := &Cache{path: path, f: file{Version: fileVersion, HubURL: hubURL, Records: map[string]Entry{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("cache unreadable, discarding it", "path", path, "err", err)
		}
		return c
	}
	var f file
	switch err := json.Unmarshal(b, &f); {
	case err != nil:
		log.Warn("cache unreadable, discarding it", "path", path, "err", err)
	case f.Version != fileVersion:
		log.Warn("cache has an unknown version, discarding it", "path", path, "version", f.Version)
	case f.HubURL != hubURL:
		log.Info("cache is for another hub_url, discarding it", "path", path, "cached", f.HubURL)
	default:
		if f.Records == nil {
			f.Records = map[string]Entry{}
		}
		c.f = f
	}
	return c
}

// Get returns the entry for key.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.f.Records[key]
	return e, ok
}

// Set records the acked state for key.
func (c *Cache) Set(key string, e Entry) {
	e.SrcMtime = e.SrcMtime.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.f.Records[key] = e
	c.dirty = true
}

// Prune drops the entries of source whose Record key is not in present:
// records that have gone from disk.
func (c *Cache) Prune(source string, present map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := source + "\x00"
	for k := range c.f.Records {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix && !present[k[len(prefix):]] {
			delete(c.f.Records, k)
			c.dirty = true
		}
	}
}

// Dirty reports whether the cache has changed since it was last saved.
func (c *Cache) Dirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// Save writes the cache atomically: a temp file, then a rename.
func (c *Cache) Save() error {
	c.mu.Lock()
	b, err := json.Marshal(c.f)
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeAtomic(c.path, b); err != nil {
		c.mu.Lock()
		c.dirty = true
		c.mu.Unlock()
		return err
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
