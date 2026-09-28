package ohmypi

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tedkulp/agent-history/internal/collector/drift"
	"github.com/tedkulp/agent-history/internal/collector/source"
)

const (
	session  = "-src-app/2026-09-17T17-58-38-183Z_01a0b085-1516-7000-9081-e596e495299d"
	session2 = "-src-app/2026-09-18T09-00-00-000Z_01a0c000-1111-7222-8333-944455556666"
	mainKey  = session + ".jsonl"
	childKey = session + "/T3SwitchResearch.jsonl"
	grandKey = session + "/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch.jsonl"
	otherKey = session2 + ".jsonl"
)

func header(cwd string) string {
	return `{"type":"title","v":1,"title":"","updatedAt":"2026-09-17T17:58:38.183Z","pad":"          "}` + "\n" +
		`{"type":"session","version":3,"id":"01a0b085-1516-7000-9081-e596e495299d","timestamp":"2026-09-17T17:58:38.183Z","cwd":"` + cwd + `"}` + "\n"
}

func write(t *testing.T, root, rel, body string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func gz(t *testing.T, b string) string {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(b)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// The root is the first candidate holding sessions/, checked against omp
// 18.3.4's dirs.ts (adapter spec §2.1).
func TestDefaultRoot(t *testing.T) {
	home := t.TempDir()
	mk := func(dir string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	xdg := t.TempDir()
	custom := mk(t.TempDir())
	xdgRoot := mk(filepath.Join(xdg, "omp"))
	xdgProfile := mk(filepath.Join(xdg, "omp", "profiles", "work"))
	homeProfile := mk(filepath.Join(home, ".omp", "profiles", "play", "agent"))
	mk(filepath.Join(home, ".omp", "agent"))
	cfgDir := mk(filepath.Join(home, ".config", "omp", "agent"))

	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", nil, filepath.Join(home, ".omp", "agent")},
		{"PI_CODING_AGENT_DIR", map[string]string{"PI_CODING_AGENT_DIR": custom, "XDG_DATA_HOME": xdg}, custom},
		{"a profile wins over PI_CODING_AGENT_DIR", map[string]string{"PI_CODING_AGENT_DIR": custom, "OMP_PROFILE": "play"}, homeProfile},
		{"a profile under XDG", map[string]string{"OMP_PROFILE": "work", "XDG_DATA_HOME": xdg}, xdgProfile},
		{"the legacy PI_PROFILE", map[string]string{"PI_PROFILE": "play", "XDG_DATA_HOME": xdg}, homeProfile},
		{"the default profile is none", map[string]string{"OMP_PROFILE": "default", "XDG_DATA_HOME": xdg}, xdgRoot},
		{"XDG_DATA_HOME", map[string]string{"XDG_DATA_HOME": xdg, "XDG_STATE_HOME": t.TempDir()}, xdgRoot},
		{"PI_CONFIG_DIR is under home", map[string]string{"PI_CONFIG_DIR": ".config/omp"}, cfgDir},
		{"a candidate without sessions/ is skipped", map[string]string{"PI_CODING_AGENT_DIR": t.TempDir()}, filepath.Join(home, ".omp", "agent")},
		{"not detected: the profile's default", map[string]string{"OMP_PROFILE": "none"}, filepath.Join(home, ".omp", "profiles", "none", "agent")},
	} {
		getenv := func(k string) string { return c.env[k] }
		if got := (Adapter{}).DefaultRoot(getenv, home); got != c.want {
			t.Errorf("%s: root %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDetect(t *testing.T) {
	if (Adapter{}).Detect(t.TempDir()) {
		t.Fatal("a root with no sessions directory detected")
	}
	for _, d := range []string{"sessions", "archive/sessions"} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if !(Adapter{}).Detect(root) {
			t.Errorf("root with %s/ not detected", d)
		}
	}
}

// Keys are relative to sessions/ or archive/sessions/ without .gz; the live
// copy wins over the archived one (adapter spec §2.2).
func TestDiscoverKeys(t *testing.T) {
	root := t.TempDir()
	body := header("/src/app")
	mainPath := write(t, root, "sessions/"+mainKey, body)
	write(t, root, "archive/sessions/"+mainKey+".gz", gz(t, body))
	childPath := write(t, root, "sessions/"+childKey, body)
	grandPath := write(t, root, "sessions/"+grandKey, body)
	otherPath := write(t, root, "archive/sessions/"+otherKey+".gz", gz(t, body))
	// Not Sessions.
	write(t, root, "sessions/"+session+"/local/legacy-memories.jsonl", "{}\n")
	write(t, root, "sessions/"+session+"/T3SwitchResearch/Other.jsonl", "{}\n")
	write(t, root, "sessions/-src-app/notes.jsonl", "{}\n")
	write(t, root, "sessions/"+session+".jsonl.1.bak", body)

	recs, err := jsonlLayout{}.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.Record{
		{Key: mainKey, Path: mainPath},
		{Key: childKey, Path: childPath},
		{Key: grandKey, Path: grandPath},
		{Key: otherKey, Path: otherPath},
	}
	slices.SortFunc(want, func(a, b source.Record) int {
		if a.Key < b.Key {
			return -1
		}
		return 1
	})
	if !slices.Equal(recs, want) {
		t.Fatalf("records:\n got  %+v\n want %+v", recs, want)
	}
	if _, err := (jsonlLayout{}).Discover(t.TempDir()); err != nil {
		t.Errorf("an empty root: %v", err)
	}
}

func TestClaims(t *testing.T) {
	root := t.TempDir()
	archived := write(t, root, "archive/sessions/"+mainKey+".gz", gz(t, "x\n"))
	if rec, ok := (jsonlLayout{}).Claims(root, archived); !ok || rec.Key != mainKey || rec.Path != archived {
		t.Fatalf("Claims(archived) = %+v, %v", rec, ok)
	}
	live := write(t, root, "sessions/"+mainKey, "x\n")
	if rec, ok := (jsonlLayout{}).Claims(root, archived); !ok || rec.Path != live {
		t.Errorf("Claims(archived) next to its live copy = %+v, %v; want the live copy", rec, ok)
	}
	for _, p := range []string{
		filepath.Join(root, mainKey), // outside the sessions directories
		filepath.Join(root, "sessions", session, "local", "x.jsonl"),
		filepath.Join(root, "sessions", "-src-app", "2026-09-17_nope.jsonl"),
		filepath.Join(t.TempDir(), "sessions", mainKey), // another root
	} {
		if _, ok := (jsonlLayout{}).Claims(root, p); ok {
			t.Errorf("Claims(%q) = true", p)
		}
	}
}

// Lock files, backups and artifact outputs are known-ignored; nothing in the
// scan is unclaimed (adapter spec §2.2).
func TestDrift(t *testing.T) {
	root := t.TempDir()
	body := header("/src/app")
	write(t, root, "sessions/"+mainKey, body)
	write(t, root, "sessions/"+childKey, body)
	write(t, root, "sessions/"+grandKey, body)
	ignored := []string{
		"sessions/-src-app/." + filepath.Base(mainKey) + ".lock",
		"sessions/-src-app/." + filepath.Base(mainKey) + ".lock.os",
		"sessions/" + mainKey + ".1788636252339.bak",
		"sessions/" + session + "/T3SwitchResearch.md",
		"sessions/" + session + "/T3SwitchResearch.json",
		"sessions/" + session + "/12.bash.log",
		"sessions/" + session + "/12.bash-original.log",
		"sessions/" + session + "/3.eval.log",
		"sessions/" + session + "/4.read.log",
		"sessions/" + session + "/local/legacy-memories.jsonl",
		"sessions/" + session + "/local/resolution.txt",
		"sessions/" + session + "/url-search/abc.json",
		"sessions/" + session + "/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch.md",
		"archive/sessions/" + session + "/T3SwitchResearch.md",
	}
	for _, p := range ignored {
		write(t, root, p, "x")
	}
	write(t, root, "agent.db", "x")
	write(t, root, "config.yml", "x")
	// Outside an artifacts directory, an unknown file is drift.
	write(t, root, "sessions/-src-app/notes.md", "x")

	r := drift.Scan(Adapter{}, root)
	if want := []string{"sessions/-src-app/notes.md"}; !slices.Equal(r.Unclaimed, want) {
		t.Errorf("unclaimed %q, want %q", r.Unclaimed, want)
	}
	var got []string
	for _, p := range r.Ignored {
		got = append(got, p.Path)
	}
	slices.Sort(got)
	slices.Sort(ignored)
	if !slices.Equal(got, ignored) {
		t.Errorf("ignored:\n got  %q\n want %q", got, ignored)
	}
}

func TestStartCwd(t *testing.T) {
	root := t.TempDir()
	p := write(t, root, "sessions/"+mainKey, header("/Users/ted/src/app"))
	if cwd, err := (jsonlLayout{}).StartCwd(source.Record{Key: mainKey, Path: p}); err != nil || cwd != "/Users/ted/src/app" {
		t.Errorf("StartCwd = %q, %v", cwd, err)
	}
	gzPath := write(t, root, "archive/sessions/"+otherKey+".gz", gz(t, header("/Users/ted/src/other")))
	if cwd, err := (jsonlLayout{}).StartCwd(source.Record{Key: otherKey, Path: gzPath}); err != nil || cwd != "/Users/ted/src/other" {
		t.Errorf("StartCwd(archived) = %q, %v", cwd, err)
	}
	// A header still being written isn't read yet.
	half := write(t, root, "sessions/"+session2+".jsonl", `{"type":"title","v":1,"title":""}`+"\n"+`{"type":"session","cwd":"/Us`)
	if cwd, err := (jsonlLayout{}).StartCwd(source.Record{Key: otherKey, Path: half}); err != nil || cwd != "" {
		t.Errorf("StartCwd(half-written) = %q, %v", cwd, err)
	}
	// A v1 file has no title slot.
	v1 := write(t, root, "sessions/-src-app/2026-01-01T00-00-00-000Z_01a0b085-1516-7000-9081-000000000001.jsonl", `{"type":"session","version":1,"cwd":"/src/v1"}`+"\n")
	if cwd, _ := (jsonlLayout{}).StartCwd(source.Record{Path: v1}); cwd != "/src/v1" {
		t.Errorf("StartCwd(v1) = %q", cwd)
	}
}

// A sub-agent's parent is its key with the last segment removed, in the
// parent's preferred copy (adapter spec §2.4).
func TestParent(t *testing.T) {
	root := t.TempDir()
	mainPath := write(t, root, "archive/sessions/"+mainKey+".gz", gz(t, header("/src/app")))
	childPath := write(t, root, "sessions/"+childKey, header("/src/app"))
	grandPath := write(t, root, "sessions/"+grandKey, header("/src/app"))

	l := jsonlLayout{}
	if rec, ok := l.Parent(root, source.Record{Key: grandKey, Path: grandPath}); !ok || rec.Key != childKey || rec.Path != childPath {
		t.Errorf("Parent(grandchild) = %+v, %v", rec, ok)
	}
	if rec, ok := l.Parent(root, source.Record{Key: childKey, Path: childPath}); !ok || rec.Key != mainKey || rec.Path != mainPath {
		t.Errorf("Parent(child) = %+v, %v", rec, ok)
	}
	if rec, ok := l.Parent(root, source.Record{Key: mainKey, Path: mainPath}); ok {
		t.Errorf("Parent(top-level) = %+v", rec)
	}
}
