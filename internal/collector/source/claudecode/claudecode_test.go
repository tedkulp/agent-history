package claudecode

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const sess = "5f1c9a2e-8d1b-4f7a-9c2e-5b0d7e1a90e2"

func writeFiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverRecordKeys(t *testing.T) {
	root := t.TempDir()
	proj := "-Users-ted-src-app"
	claimed := []string{
		proj + "/" + sess + ".jsonl",
		proj + "/" + sess + "/tool-results/toolu_01.txt",
		proj + "/" + sess + "/tool-results/p1/page-1.jpg",
		proj + "/" + sess + "/custom-title.json",
		proj + "/" + sess + ".orphaned-1790000000-ab12.jsonl",
		proj + "/" + sess + ".jsonl.superseded-1790000000",
		proj + "/" + sess + "/subagents/agent-a1b2c3.jsonl",
		proj + "/" + sess + "/subagents/agent-a1b2c3.meta.json",
	}
	ignored := []string{
		proj + "/memory/MEMORY.md",
		proj + "/not-a-uuid.jsonl",
		proj + "/" + sess + "/notes.txt",
		proj + "/" + sess + "/subagents/other.jsonl",
		"stray.jsonl",
	}
	writeFiles(t, root, append(claimed, ignored...)...)

	recs, err := jsonlLayout{}.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range recs {
		keys = append(keys, r.Key)
		if want := filepath.Join(root, filepath.FromSlash(r.Key)); r.Path != want {
			t.Errorf("path of %q = %q, want %q", r.Key, r.Path, want)
		}
	}
	slices.Sort(keys)
	slices.Sort(claimed)
	if !slices.Equal(keys, claimed) {
		t.Fatalf("keys:\n got  %q\n want %q", keys, claimed)
	}
}

func TestDefaultRoot(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := (Adapter{}).DefaultRoot(getenv, "/home/ted"); got != "/home/ted/.claude/projects" {
		t.Fatalf("default root %q", got)
	}
	env["CLAUDE_CONFIG_DIR"] = "/opt/claude"
	if got := (Adapter{}).DefaultRoot(getenv, "/home/ted"); got != "/opt/claude/projects" {
		t.Fatalf("CLAUDE_CONFIG_DIR root %q", got)
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	if !(Adapter{}).Detect(root) {
		t.Fatal("existing dir not detected")
	}
	if (Adapter{}).Detect(filepath.Join(root, "missing")) {
		t.Fatal("missing dir detected")
	}
}

func TestClaims(t *testing.T) {
	root := "/r/a"
	key := "-Users-ted-src-app/" + sess + ".jsonl"
	rec, ok := jsonlLayout{}.Claims(root, root+"/"+key)
	if !ok || rec.Key != key || rec.Path != root+"/"+key {
		t.Fatalf("Claims = %+v, %v", rec, ok)
	}
	for _, p := range []string{"/r/a/-Users-ted-src-app/memory/MEMORY.md", "/elsewhere/-p/" + sess + ".jsonl", "/r/" + sess + ".jsonl", "/r/a"} {
		if _, ok := (jsonlLayout{}).Claims(root, p); ok {
			t.Errorf("Claims(%q) = true", p)
		}
	}
}
