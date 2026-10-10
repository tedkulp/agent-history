package hermes

import (
	"database/sql"
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source"
)

// schema is Hermes's schema_version 30 tables, as the export reads them.
//
//go:embed testdata/schema.sql
var schema string

const (
	cliID      = "20261001_090000_a1b2c3"
	desktopID  = "20261001_100000_d4e5f6"
	childID    = "20261001_090500_c0ffee"
	grandID    = "20261001_090600_beef01"
	gatewayID  = "20261001_110000_9a8b7c"
	cronID     = "20261001_120000_1d2e3f"
	strayChild = "20261001_110500_5a5a5a"
	continued  = "20261001_130000_c0c0c0"
)

// newDB creates a Hermes state.db in WAL mode under root, holding a cli
// Session with a subagent child and grandchild, a desktop Session, and a
// gateway and a cron Session with a subagent of their own. It returns a
// writer connection held open as Hermes's would be.
func newDB(t *testing.T, root string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	t.Cleanup(func() { w.Close() })
	exec(t, w, `PRAGMA journal_mode=WAL`)
	exec(t, w, schema)
	exec(t, w, `INSERT INTO schema_version VALUES (30)`)
	for _, s := range []struct {
		id, src, parent, cwd, repo string
		started                    float64
	}{
		{cliID, "cli", "", "/Users/ted/src/app", "/Users/ted/src/app", 1000.5},
		{desktopID, "desktop", "", "", "/Users/ted/src/site", 1100},
		{childID, "subagent", cliID, "/Users/ted/src/app/sub", "", 1010},
		{grandID, "subagent", childID, "", "", 1020},
		{gatewayID, "mattermost", "", "", "", 1200},
		{cronID, "cron", "", "", "", 1300},
		{strayChild, "subagent", gatewayID, "", "", 1210},
		{continued, "cli", cliID, "/elsewhere", "", 1400},
	} {
		exec(t, w, `INSERT INTO sessions (id, source, parent_session_id, cwd, git_repo_root, started_at, title)
		  VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`, s.id, s.src, s.parent, s.cwd, s.repo, s.started, "Plan <the> work "+s.id)
	}
	exec(t, w, `INSERT INTO messages (session_id, role, content, timestamp, display_order) VALUES
	  (?, 'user', 'hi & bye', 1001.25, 1), (?, 'assistant', 'hello', 1002.5, 2)`, cliID, cliID)
	return w
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T, root string) []source.Record {
	t.Helper()
	recs, err := Adapter{}.Layouts()[0].Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func record(t *testing.T, recs []source.Record, key string) source.Record {
	t.Helper()
	for _, r := range recs {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no record %s", key)
	return source.Record{}
}

func TestDefaultRoot(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := (Adapter{}).DefaultRoot(env(nil), "/Users/ted"); got != "/Users/ted/.hermes" {
		t.Errorf("default root = %s", got)
	}
	if got := (Adapter{}).DefaultRoot(env(map[string]string{"HERMES_HOME": "/h"}), "/Users/ted"); got != "/h" {
		t.Errorf("HERMES_HOME root = %s", got)
	}
}

// Only cli and desktop Sessions are collected, with the subagent Sessions
// under them at any depth. Gateway and cron Sessions, and their subagents,
// are not.
func TestDiscoverEntryPoints(t *testing.T) {
	root := t.TempDir()
	newDB(t, root)
	if !(Adapter{}).Detect(root) || (Adapter{}).Detect(t.TempDir()) {
		t.Error("Detect should follow state.db")
	}
	var got []string
	for _, r := range discover(t, root) {
		got = append(got, r.Key)
	}
	if want := []string{cliID, childID, grandID, desktopID, continued}; !slices.Equal(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
}

// The export is the sessions row then the messages rows in id order, every
// column, and is byte-identical when nothing changed.
func TestExport(t *testing.T) {
	root := t.TempDir()
	newDB(t, root)
	rec := record(t, discover(t, root), cliID)
	if rec.Path != filepath.Join(root, dbName) {
		t.Errorf("path = %s", rec.Path)
	}
	if !rec.Stat.Mtime.Equal(time.UnixMicro(1002_500_000)) {
		t.Errorf("mtime = %v", rec.Stat.Mtime)
	}
	b, err := source.Content(rec)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("export has %d lines:\n%s", len(lines), b)
	}
	if !strings.HasPrefix(lines[0], `{"table":"sessions","row":{"id":"`+cliID+`","source":"cli",`) ||
		!strings.Contains(lines[0], `"title":"Plan <the> work `+cliID+`"`) || !strings.Contains(lines[0], `"started_at":1000.5`) {
		t.Errorf("sessions line = %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], `{"table":"messages","row":{"id":1,"session_id":"`+cliID+`","role":"user","content":"hi & bye",`) ||
		!strings.Contains(lines[2], `"role":"assistant"`) {
		t.Errorf("messages lines = %s\n%s", lines[1], lines[2])
	}
	again, err := source.Content(record(t, discover(t, root), cliID))
	if err != nil || string(again) != string(b) {
		t.Errorf("second export differs (err %v)", err)
	}
}

// An in-place rewrite changes the change signal: compaction flipping a
// row's flags, and an edit to its content, though neither adds a row nor
// a newer time.
func TestChangeSignal(t *testing.T) {
	root := t.TempDir()
	w := newDB(t, root)
	stat := func() source.Stat { return record(t, discover(t, root), cliID).Stat }
	before := stat()
	exec(t, w, `UPDATE messages SET active = 0, compacted = 1 WHERE id = 1`)
	compacted := stat()
	if compacted == before {
		t.Error("compaction left the change signal unchanged")
	}
	exec(t, w, `UPDATE messages SET content = 'hello there' WHERE id = 2`)
	edited := stat()
	if edited == compacted {
		t.Error("an edit left the change signal unchanged")
	}
	// Opposite changes to two rows don't cancel.
	exec(t, w, `UPDATE messages SET active = 1, compacted = 0 WHERE id = 1`)
	exec(t, w, `UPDATE messages SET active = 0, compacted = 1 WHERE id = 2`)
	if stat() == edited {
		t.Error("swapped flags left the change signal unchanged")
	}
}

// StartCwd is cwd, else git_repo_root, else empty. Parent walks up through
// subagent Sessions.
func TestStartCwdAndParent(t *testing.T) {
	root := t.TempDir()
	newDB(t, root)
	l := Adapter{}.Layouts()[0]
	recs := discover(t, root)
	for key, want := range map[string]string{cliID: "/Users/ted/src/app", desktopID: "/Users/ted/src/site", grandID: ""} {
		if got, err := l.StartCwd(record(t, recs, key)); err != nil || got != want {
			t.Errorf("StartCwd(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	p, ok := l.Parent(root, record(t, recs, grandID))
	if !ok || p.Key != childID {
		t.Fatalf("grandchild's parent = %+v, %v", p, ok)
	}
	if cwd, _ := l.StartCwd(p); cwd != "/Users/ted/src/app/sub" {
		t.Errorf("parent's cwd = %q", cwd)
	}
	pp, ok := l.Parent(root, p)
	if !ok || pp.Key != cliID {
		t.Errorf("child's parent = %+v, %v", pp, ok)
	}
	if _, ok := l.Parent(root, record(t, recs, cliID)); ok {
		t.Error("a top-level Session has a parent")
	}
	// A compression continuation names its parent, but isn't its Child
	// Session: excluding one doesn't exclude the other.
	if p, ok := l.Parent(root, record(t, recs, continued)); ok {
		t.Errorf("a cli Session's parent = %+v", p)
	}
}

func TestMissingTable(t *testing.T) {
	root := t.TempDir()
	w := newDB(t, root)
	exec(t, w, `DROP TABLE messages`)
	if _, err := (Adapter{}).Layouts()[0].Discover(root); err == nil || !strings.Contains(err.Error(), "no messages table") {
		t.Errorf("err = %v", err)
	}
}
