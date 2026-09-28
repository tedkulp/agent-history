package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/collector/drift"
	"github.com/tedkulp/agent-history/internal/collector/source"
)

const (
	thread  = "01a0d387-4530-74a2-8c7a-d5f10cc15942"
	thread2 = "01a0a139-c023-7610-8e4a-f7104bf801f1"
	child   = "01a0d390-1111-7222-8333-944455556666"
	rollout = "01a0d399-aaaa-7bbb-8ccc-9ddddeeeeeee"
	day     = "sessions/2026/09/24/"
)

func name(ts, id string) string { return "rollout-" + ts + "-" + id + ".jsonl" }

var (
	mainKey  = name("2026-09-24T09-07-32", thread)
	contKey  = name("2026-09-24T10-00-00", thread+"_"+rollout)
	childKey = name("2026-09-24T09-30-00", child)
	otherKey = name("2026-09-14T14-41-51", thread2)
)

func metaLine(id, cwd, parent string) string {
	p := ""
	if parent != "" {
		p = `,"parent_thread_id":"` + parent + `"`
	}
	return `{"timestamp":"2026-09-24T13:08:26.379Z","ordinal":0,"type":"session_meta","payload":{"session_id":"` + id + `","id":"` + id + `"` + p + `,"timestamp":"2026-09-24T13:07:32.785Z","cwd":"` + cwd + `","originator":"codex-tui","cli_version":"0.156.1","source":"cli","model_provider":"openai","base_instructions":{"text":"redacted"}}}` + "\n"
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

func compress(t *testing.T, b string) string {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(enc.EncodeAll([]byte(b), nil))
}

func TestDefaultRoot(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := (Adapter{}).DefaultRoot(getenv, "/home/ted"); got != "/home/ted/.codex" {
		t.Fatalf("default root %q", got)
	}
	env["CODEX_HOME"] = "/opt/codex"
	if got := (Adapter{}).DefaultRoot(getenv, "/home/ted"); got != "/opt/codex" {
		t.Fatalf("CODEX_HOME root %q", got)
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	if (Adapter{}).Detect(root) {
		t.Fatal("a root with no history directory detected")
	}
	for _, d := range []string{"archived_sessions", "sessions"} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if !(Adapter{}).Detect(root) {
			t.Errorf("root with %s/ not detected", d)
		}
	}
}

// Keys are basenames without .zst; when one key has several copies, the
// record holds the preferred one (adapter spec §2.2).
func TestDiscoverKeysAndPreference(t *testing.T) {
	root := t.TempDir()
	body := metaLine(thread, "/src/app", "")
	// mainKey: plain and compressed under sessions/, plus an archived copy.
	plain := write(t, root, day+mainKey, body)
	write(t, root, day+mainKey+".zst", compress(t, body))
	write(t, root, "archived_sessions/"+mainKey, body)
	// contKey: compressed under sessions/ and plain under archived_sessions/.
	contZst := write(t, root, day+contKey+".zst", compress(t, body))
	write(t, root, "archived_sessions/"+contKey, body)
	// otherKey: archived only, compressed.
	otherZst := write(t, root, "archived_sessions/"+otherKey+".zst", compress(t, body))
	// Not rollouts.
	write(t, root, day+"rollout-bad-"+thread+".jsonl", body)
	write(t, root, day+mainKey+".zst.compress.1.0.tmp", "x")
	write(t, root, "history.jsonl", "{}\n")
	write(t, root, "session_index.jsonl", "{}\n")

	recs, err := jsonlLayout{}.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []source.Record{
		{Key: otherKey, Path: otherZst},
		{Key: mainKey, Path: plain},
		{Key: contKey, Path: contZst},
	}
	slices.SortFunc(want, func(a, b source.Record) int { return strings.Compare(a.Key, b.Key) })
	if !slices.Equal(recs, want) {
		t.Fatalf("records:\n got  %+v\n want %+v", recs, want)
	}

	if _, err := (jsonlLayout{}).Discover(t.TempDir()); err != nil {
		t.Errorf("an empty root: %v", err)
	}
}

func TestClaims(t *testing.T) {
	root := t.TempDir()
	zst := write(t, root, day+mainKey+".zst", compress(t, "x\n"))
	rec, ok := jsonlLayout{}.Claims(root, zst)
	if !ok || rec.Key != mainKey || rec.Path != zst {
		t.Fatalf("Claims(zst) = %+v, %v", rec, ok)
	}
	plain := write(t, root, day+mainKey, "x\n")
	if rec, ok := (jsonlLayout{}).Claims(root, zst); !ok || rec.Path != plain {
		t.Errorf("Claims(zst) next to its plain copy = %+v, %v; want the plain copy", rec, ok)
	}
	for _, p := range []string{
		filepath.Join(root, mainKey),                           // outside the history directories
		filepath.Join(root, "sessions", "history.jsonl"),       // not a rollout
		filepath.Join(root, "thread_history_1.sqlite"),         // known-ignored
		filepath.Join(t.TempDir(), "sessions", mainKey),        // another root
		filepath.Join(root, day, "rollout-x-"+thread+".jsonl"), // bad timestamp
	} {
		if _, ok := (jsonlLayout{}).Claims(root, p); ok {
			t.Errorf("Claims(%q) = true", p)
		}
	}
}

func TestStartCwd(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct{ name, body, want string }{
		{"empty", "", ""},
		{"partial first line waits", metaLine(thread, "/src/app", "")[:40], ""},
		{"session_meta", metaLine(thread, "/src/app", "") + `{"type":"turn_context","payload":{"cwd":"/elsewhere"}}` + "\n", "/src/app"},
		{"first line not session_meta", `{"type":"turn_context","payload":{"cwd":"/x"}}` + "\n", ""},
	} {
		p := write(t, root, day+mainKey, c.body)
		got, err := jsonlLayout{}.StartCwd(source.Record{Key: mainKey, Path: p})
		if err != nil || got != c.want {
			t.Errorf("%s: StartCwd = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	zst := write(t, root, "archived_sessions/"+otherKey+".zst", compress(t, metaLine(thread2, "/src/other", "")))
	if got, err := (jsonlLayout{}).StartCwd(source.Record{Key: otherKey, Path: zst}); err != nil || got != "/src/other" {
		t.Errorf("compressed: StartCwd = %q, %v", got, err)
	}
}

func TestParent(t *testing.T) {
	root := t.TempDir()
	mainPath := write(t, root, "archived_sessions/"+mainKey+".zst", compress(t, metaLine(thread, "/src/app", "")))
	cont := write(t, root, day+contKey, metaLine(thread, "/src/app", ""))
	sub := write(t, root, day+childKey, metaLine(child, "/src/app", thread))
	other := write(t, root, day+otherKey, metaLine(thread2, "/src/other", ""))

	want := source.Record{Key: mainKey, Path: mainPath}
	for _, rec := range []source.Record{{Key: contKey, Path: cont}, {Key: childKey, Path: sub}} {
		if p, ok := (jsonlLayout{}).Parent(root, rec); !ok || p != want {
			t.Errorf("Parent(%s) = %+v, %v; want %+v", rec.Key, p, ok, want)
		}
	}
	// The preferred copy wins once there is one.
	plain := write(t, root, day+mainKey, metaLine(thread, "/src/app", ""))
	if p, ok := (jsonlLayout{}).Parent(root, source.Record{Key: contKey, Path: cont}); !ok || p.Path != plain {
		t.Errorf("Parent(cont) = %+v, %v; want %s", p, ok, plain)
	}
	for _, rec := range []source.Record{{Key: mainKey, Path: plain}, {Key: otherKey, Path: other}} {
		if p, ok := (jsonlLayout{}).Parent(root, rec); ok {
			t.Errorf("Parent(%s) = %+v, want none", rec.Key, p)
		}
	}
	// A sub-agent whose parent isn't on disk has none.
	orphan := write(t, root, day+name("2026-09-24T11-00-00", rollout), metaLine(rollout, "/src/app", thread2+"0"))
	if p, ok := (jsonlLayout{}).Parent(root, source.Record{Key: filepath.Base(orphan), Path: orphan}); ok {
		t.Errorf("Parent(orphan) = %+v, want none", p)
	}
}

// The thread store and its WAL files are known-ignored; nothing else under
// the root outside the history directories is reported (adapter spec §2.2).
func TestDriftScan(t *testing.T) {
	root := t.TempDir()
	write(t, root, day+mainKey, "x\n")
	write(t, root, "thread_history_1.sqlite", "x")
	write(t, root, "thread_history_1.sqlite-wal", "x")
	write(t, root, "state_5.sqlite", "x")
	write(t, root, "config.toml", "x")
	write(t, root, "session_index.jsonl", "x")
	write(t, root, "log/codex-tui.log", "x")
	write(t, root, day+"notes.txt", "x")

	res := drift.Scan(Adapter{}, root)
	if !slices.Equal(res.Unclaimed, []string{day + "notes.txt"}) {
		t.Errorf("unclaimed %q", res.Unclaimed)
	}
	var ignored []string
	for _, p := range res.Ignored {
		ignored = append(ignored, p.Path)
	}
	slices.Sort(ignored)
	if want := []string{"thread_history_1.sqlite", "thread_history_1.sqlite-wal"}; !slices.Equal(ignored, want) {
		t.Errorf("ignored %q, want %q", ignored, want)
	}
}

// A record claimed through its archived copy holds the copy under
// sessions/ when there is one.
func TestClaimsPrefersSessions(t *testing.T) {
	root := t.TempDir()
	archived := write(t, root, "archived_sessions/"+mainKey, "x\n")
	if rec, ok := (jsonlLayout{}).Claims(root, archived); !ok || rec.Path != archived {
		t.Fatalf("Claims(archived) = %+v, %v", rec, ok)
	}
	zst := write(t, root, day+mainKey+".zst", compress(t, "x\n"))
	if rec, ok := (jsonlLayout{}).Claims(root, archived); !ok || rec.Key != mainKey || rec.Path != zst {
		t.Errorf("Claims(archived) = %+v, %v; want %s", rec, ok, zst)
	}
}
