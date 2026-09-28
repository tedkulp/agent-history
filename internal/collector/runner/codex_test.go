package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/collector/source/codex"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	cxThread = "01a0d387-4530-74a2-8c7a-d5f10cc15942"
	cxChild  = "01a0d390-1111-7222-8333-944455556666"
	cxOther  = "01a0a139-c023-7610-8e4a-f7104bf801f1"
	cxDay    = "sessions/2026/09/24/"
	cxKey    = "rollout-2026-09-24T09-07-32-" + cxThread + ".jsonl"
	cxChildK = "rollout-2026-09-24T09-30-00-" + cxChild + ".jsonl"
	cxOtherK = "rollout-2026-09-24T10-00-00-" + cxOther + ".jsonl"
)

func cxMeta(id, cwd, parent string) string {
	p := ""
	if parent != "" {
		p = `,"parent_thread_id":"` + parent + `"`
	}
	return `{"timestamp":"2026-09-24T13:08:26.379Z","ordinal":0,"type":"session_meta","payload":{"id":"` + id + `"` + p + `,"cwd":"` + cwd + `"}}` + "\n"
}

// hubHolds reports whether the Hub's current version of key is body.
func (f *fixture) hubHolds(key, body string) bool {
	f.t.Helper()
	m, err := f.store.Manifest(context.Background(), machine, "")
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	for _, r := range m {
		if r.RecordKey == key {
			return r.Length == int64(len(body)) && r.Sha256 == hex.EncodeToString(sum[:])
		}
	}
	return false
}

// records counts the Hub's records of one Source.
func (f *fixture) records(source string) int {
	f.t.Helper()
	m, err := f.store.Manifest(context.Background(), machine, source)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(m)
}

// A live rollout ships as appends under its basename; compressing or
// archiving it ships nothing and makes no second record (codex.md §6).
func TestCodexRolloutLifecycle(t *testing.T) {
	f := newFixture(t)
	body := cxMeta(cxThread, "/Users/ted/src/app", "")
	f.write(cxDay+cxKey, body)
	f.write("thread_history_1.sqlite", "x")

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: codex.Adapter{}, Root: f.root}}
	cfg.RescanInterval = 50 * time.Millisecond
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the rollout to ship", func() bool { return f.hubHolds(cxKey, body) })

	line := `{"timestamp":"2026-09-24T13:08:30.000Z","ordinal":1,"type":"event_msg","payload":{"type":"task_started"}}`
	f.appendLine(cxDay+cxKey, line)
	body += line + "\n"
	waitFor(t, 5*time.Second, "the appended line to ship", func() bool { return f.hubHolds(cxKey, body) })

	rep := f.status(cfg.Control)
	s := rep.Sources[0]
	if s.ID != protocol.SourceCodex || s.Root != f.root || len(s.Layouts) != 1 || s.Layouts[0] != "jsonl" || s.Records != 1 {
		t.Errorf("status source %+v", s)
	}
	if s.Ignored != 1 || s.IgnoredPaths[0].Path != "thread_history_1.sqlite" || s.Unclaimed != 0 {
		t.Errorf("status ignored %d %+v, unclaimed %q", s.Ignored, s.IgnoredPaths, s.UnclaimedPaths)
	}

	posts := f.posts.Load()
	// Codex compresses a cold rollout, then it is archived.
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.write(cxDay+cxKey+".zst", string(enc.EncodeAll([]byte(body), nil)))
	if err := os.Remove(f.path(cxDay + cxKey)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	archived := f.path("archived_sessions/" + cxKey + ".zst")
	if err := os.MkdirAll(filepath.Dir(archived), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path(cxDay+cxKey+".zst"), archived); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := f.posts.Load() - posts; n != 0 {
		t.Errorf("compressing and archiving sent %d records requests, want none", n)
	}
	if n := f.records(protocol.SourceCodex); n != 1 || !f.hubHolds(cxKey, body) {
		t.Errorf("hub holds %d codex records, current matches: %v", n, f.hubHolds(cxKey, body))
	}
}

// An excluded Session's Child Sessions stay on the Machine too.
func TestCodexExcludeCoversChildSessions(t *testing.T) {
	f := newFixture(t)
	f.write(cxDay+cxKey, cxMeta(cxThread, "/Users/ted/src/secret/app", ""))
	// A sub-agent's own cwd doesn't save it.
	f.write(cxDay+cxChildK, cxMeta(cxChild, "/Users/ted/src/app", cxThread))
	open := cxMeta(cxOther, "/Users/ted/src/app", "")
	f.write(cxDay+cxOtherK, open)

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: codex.Adapter{}, Root: f.root}}
	cfg.Exclude = excludeFilter(t)
	cfg.RescanInterval = 50 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "the open thread to ship", func() bool { return f.hubHolds(cxOtherK, open) })
	time.Sleep(200 * time.Millisecond)

	for _, k := range []string{cxKey, cxChildK} {
		if f.onHub(k) {
			t.Errorf("excluded record %s reached the Hub", k)
		}
	}
	if n := cfg.Exclude.Count(protocol.SourceCodex); n != 2 {
		t.Errorf("excluded count = %d, want 2", n)
	}
}
