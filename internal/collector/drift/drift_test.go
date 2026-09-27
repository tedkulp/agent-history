package drift

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/collector/status"
)

const sess = "5f1c9a2e-8d1b-4f7a-9c2e-5b0d7e1a90e2"

func write(t *testing.T, root, rel string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// Every path in the adapters/claude-code.md §2.2 table is claimed, memory
// notes are known-ignored, and anything else is unclaimed.
func TestScanClaudeCode(t *testing.T) {
	root := t.TempDir()
	proj := "-Users-ted-src-app"
	t0 := time.Date(2026, 9, 25, 18, 2, 0, 0, time.UTC)
	for _, rel := range []string{
		proj + "/" + sess + ".jsonl",
		proj + "/" + sess + "/tool-results/toolu_01.txt",
		proj + "/" + sess + "/tool-results/p1/page-1.jpg",
		proj + "/" + sess + "/custom-title.json",
		proj + "/" + sess + ".orphaned-1790000000-ab12.jsonl",
		proj + "/" + sess + ".jsonl.superseded-1790000000",
		proj + "/" + sess + "/subagents/agent-a1b2c3.jsonl",
		proj + "/" + sess + "/subagents/agent-a1b2c3.meta.json",
	} {
		write(t, root, rel, t0)
	}
	write(t, root, proj+"/memory/MEMORY.md", t0)
	write(t, root, proj+"/memory/sub/note.md", t0.Add(time.Hour))
	unclaimed := []string{
		proj + "/" + sess + "/notes.txt",
		proj + "/not-a-uuid.jsonl",
		".DS_Store",
	}
	for _, rel := range unclaimed {
		write(t, root, rel, t0)
	}
	if err := os.Symlink(filepath.Join(root, ".DS_Store"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	got := Scan(claudecode.Adapter{}, root)
	if !slices.Equal(got.Unclaimed, unclaimed) {
		t.Errorf("unclaimed:\n got  %q\n want %q", got.Unclaimed, unclaimed)
	}
	want := []status.IgnoredPath{
		{Path: proj + "/memory/sub/note.md", Modified: t0.Add(time.Hour)},
		{Path: proj + "/memory/MEMORY.md", Modified: t0},
	}
	if len(got.Ignored) != len(want) {
		t.Fatalf("ignored = %+v, want %+v", got.Ignored, want)
	}
	for i := range want {
		if got.Ignored[i].Path != want[i].Path || !got.Ignored[i].Modified.Equal(want[i].Modified) {
			t.Errorf("ignored[%d] = %+v, want %+v", i, got.Ignored[i], want[i])
		}
	}
}

func TestScanMissingRoot(t *testing.T) {
	got := Scan(claudecode.Adapter{}, filepath.Join(t.TempDir(), "missing"))
	if len(got.Unclaimed) != 0 || len(got.Ignored) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestReportCapsPaths(t *testing.T) {
	var r Result
	for i := range 7 {
		r.Unclaimed = append(r.Unclaimed, string(rune('a'+i)))
		r.Ignored = append(r.Ignored, status.IgnoredPath{Path: string(rune('a' + i))})
	}
	var s status.Source
	r.Report(&s)
	if s.Unclaimed != 7 || !slices.Equal(s.UnclaimedPaths, []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("unclaimed %d %q", s.Unclaimed, s.UnclaimedPaths)
	}
	if s.Ignored != 7 || len(s.IgnoredPaths) != 5 || s.IgnoredPaths[0].Path != "a" {
		t.Errorf("ignored %d %+v", s.Ignored, s.IgnoredPaths)
	}
}
