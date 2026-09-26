package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func entry(n int64) Entry {
	return Entry{Length: n, Sha256: "abc", SrcSize: n + 1, SrcMtime: time.Date(2026, 9, 26, 14, 2, 11, 123e6, time.UTC)}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := Load(path, "http://hub:8080", nil)
	if c.Dirty() {
		t.Fatal("a fresh cache should not be dirty")
	}
	c.Set(Key("claude-code", "p/s.jsonl"), entry(10))
	if !c.Dirty() {
		t.Fatal("Set should mark the cache dirty")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if c.Dirty() {
		t.Fatal("Save should clear dirty")
	}

	got, ok := Load(path, "http://hub:8080", nil).Get(Key("claude-code", "p/s.jsonl"))
	if !ok || got.Length != 10 || got.SrcSize != 11 || !got.SrcMtime.Equal(entry(10).SrcMtime) {
		t.Fatalf("loaded entry %+v, ok %v", got, ok)
	}
}

func TestLoadDiscards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	c := Load(path, "http://old:8080", nil)
	c.Set(Key("claude-code", "k"), entry(1))
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(path, "http://new:8080", nil).Get(Key("claude-code", "k")); ok {
		t.Fatal("a cache for another hub_url should be discarded")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(path, "http://old:8080", nil).Get(Key("claude-code", "k")); ok {
		t.Fatal("an unreadable cache should be discarded")
	}
	if _, ok := Load(filepath.Join(dir, "missing.json"), "http://old:8080", nil).Get(Key("claude-code", "k")); ok {
		t.Fatal("a missing cache is empty")
	}
}

func TestPruneDropsOnlyThatSource(t *testing.T) {
	c := Load(filepath.Join(t.TempDir(), "cache.json"), "h", nil)
	c.Set(Key("claude-code", "keep"), entry(1))
	c.Set(Key("claude-code", "gone"), entry(1))
	c.Set(Key("codex", "other"), entry(1))
	c.Prune("claude-code", map[string]bool{"keep": true})
	for k, want := range map[string]bool{Key("claude-code", "keep"): true, Key("claude-code", "gone"): false, Key("codex", "other"): true} {
		if _, ok := c.Get(k); ok != want {
			t.Errorf("%q present = %v, want %v", k, ok, want)
		}
	}
}

func TestUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{Length: 3, SrcSize: fi.Size(), SrcMtime: fi.ModTime()}
	if !e.Unchanged(fi) {
		t.Fatal("same stat should be unchanged")
	}
	e.SrcMtime = e.SrcMtime.Add(-time.Second)
	if e.Unchanged(fi) {
		t.Fatal("different mtime should be changed")
	}
}

func TestSplitInvertsKey(t *testing.T) {
	if s, k := Split(Key("claude-code", "p/s.jsonl")); s != "claude-code" || k != "p/s.jsonl" {
		t.Fatalf("Split = %q, %q", s, k)
	}
}
