package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/drift"
	"github.com/tedkulp/agent-history/internal/collector/source"
)

// schema is opencode 1.18.31's tables, as far as the export reads them.
const schema = `
CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, parent_id text, slug text NOT NULL,
  directory text NOT NULL, title text NOT NULL, version text NOT NULL, revert text,
  time_created integer NOT NULL, time_updated integer NOT NULL, cost real DEFAULT 0 NOT NULL, icon blob);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
  time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL, seq integer NOT NULL);
`

const (
	parentID = "ses_1ba0a4a04ffeLs3vMw9nTpQx7c"
	childID  = "ses_1ba0755e0ffeMq2vLs4cTpNw8x"
)

// newDB creates an opencode database in WAL mode under root, with a parent
// Session and its Child Session, and returns a writer connection held open
// as opencode's would be.
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
	exec(t, w, `INSERT INTO session VALUES
	  (?, 'proj', NULL, 'quiet-cat', '/Users/ted/src/app', 'Plan <the> work', '1.18.31', NULL, 1000, 1500, 0.25, X'00FF'),
	  (?, 'proj', ?, 'loud-dog', '/tmp/elsewhere', 'Explore (@explore subagent)', '1.18.30', NULL, 1100, 1200, 0, NULL)`,
		parentID, childID, parentID)
	exec(t, w, `INSERT INTO message VALUES
	  ('msg_b', ?, 2000, 2000, '{"role":"assistant"}'),
	  ('msg_a', ?, 2000, 2100, '{"role":"user"}'),
	  ('msg_c', ?, 1100, 1100, '{"role":"user"}')`, parentID, parentID, childID)
	exec(t, w, `INSERT INTO part VALUES
	  ('prt_2', 'msg_a', ?, 2000, 2000, '{"type":"text","text":"hi"}'),
	  ('prt_1', 'msg_a', ?, 2000, 2600, '{"type":"text","text":"a & b"}')`, parentID, parentID)
	exec(t, w, `INSERT INTO session_message VALUES ('sm_1', ?, 'agent-switched', 2000, 2000, '{}', 1)`, parentID)
	return w
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T, l source.Layout, root string) []source.Record {
	t.Helper()
	recs, err := l.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func keys(recs []source.Record) []string {
	var ks []string
	for _, r := range recs {
		ks = append(ks, r.Key)
	}
	return ks
}

func TestDefaultRoot(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	a := Adapter{}
	if got := a.DefaultRoot(env(nil), "/Users/ted"); got != "/Users/ted/.local/share/opencode" {
		t.Errorf("default root = %s", got)
	}
	if got := a.DefaultRoot(env(map[string]string{"XDG_DATA_HOME": "/data"}), "/Users/ted"); got != "/data/opencode" {
		t.Errorf("XDG root = %s", got)
	}
	for v, want := range map[string]string{"": "", ":memory:": "", "/db/oc.db": "/db/oc.db", "alt.db": "/root/alt.db"} {
		if got := a.DefaultDB(env(map[string]string{"OPENCODE_DB": v}), "/root"); got != want {
			t.Errorf("OPENCODE_DB=%q: db = %q, want %q", v, got, want)
		}
	}
}

// The export is the Session's rows, every column in table order, rows in
// the §2.2 order, and is byte-identical when nothing changed.
func TestExport(t *testing.T) {
	root := t.TempDir()
	w := newDB(t, root)
	l := Adapter{}.Layouts()[1]
	recs := discover(t, l, root)
	if got := keys(recs); !slices.Equal(got, []string{"db:" + childID, "db:" + parentID}) {
		t.Fatalf("keys = %q", got)
	}
	p := recs[1]
	if p.Path != filepath.Join(root, dbName) {
		t.Errorf("path = %s", p.Path)
	}
	// 1 session + 2 messages + 2 parts + 1 session_message; the latest part update.
	if p.Stat.Size != 6 || !p.Stat.Mtime.Equal(time.UnixMilli(2600)) {
		t.Errorf("stat = %+v", p.Stat)
	}
	if c := recs[0]; c.Stat.Size != 2 || !c.Stat.Mtime.Equal(time.UnixMilli(1200)) {
		t.Errorf("child stat = %+v", c.Stat)
	}

	b, err := source.Content(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"table":"session","row":{"id":"` + parentID + `","project_id":"proj","parent_id":null,"slug":"quiet-cat","directory":"/Users/ted/src/app","title":"Plan <the> work","version":"1.18.31","revert":null,"time_created":1000,"time_updated":1500,"cost":0.25,"icon":{"$base64":"AP8="}}}
{"table":"message","row":{"id":"msg_a","session_id":"` + parentID + `","time_created":2000,"time_updated":2100,"data":"{\"role\":\"user\"}"}}
{"table":"message","row":{"id":"msg_b","session_id":"` + parentID + `","time_created":2000,"time_updated":2000,"data":"{\"role\":\"assistant\"}"}}
{"table":"part","row":{"id":"prt_1","message_id":"msg_a","session_id":"` + parentID + `","time_created":2000,"time_updated":2600,"data":"{\"type\":\"text\",\"text\":\"a & b\"}"}}
{"table":"part","row":{"id":"prt_2","message_id":"msg_a","session_id":"` + parentID + `","time_created":2000,"time_updated":2000,"data":"{\"type\":\"text\",\"text\":\"hi\"}"}}
{"table":"session_message","row":{"id":"sm_1","session_id":"` + parentID + `","type":"agent-switched","time_created":2000,"time_updated":2000,"data":"{}","seq":1}}
`
	if string(b) != want {
		t.Errorf("export:\n%s\nwant:\n%s", b, want)
	}
	for line := range bytes.Lines(b) {
		if !json.Valid(line) {
			t.Errorf("line isn't JSON: %s", line)
		}
	}
	again, err := source.Content(discover(t, l, root)[1])
	if err != nil || !bytes.Equal(again, b) {
		t.Errorf("second export differs (err %v)", err)
	}

	// A new message changes the signal; a deleted one too, by the row count.
	exec(t, w, `INSERT INTO message VALUES ('msg_d', ?, 3000, 3000, '{"role":"user"}')`, parentID)
	if st := discover(t, l, root)[1].Stat; st.Size != 7 || !st.Mtime.Equal(time.UnixMilli(3000)) {
		t.Errorf("stat after insert = %+v", st)
	}
	exec(t, w, `DELETE FROM message WHERE id = 'msg_d'`)
	if st := discover(t, l, root)[1].Stat; st.Size != 6 {
		t.Errorf("stat after delete = %+v", st)
	}

	// Without session_message the export skips it.
	exec(t, w, `DROP TABLE session_message`)
	b, err = source.Content(discover(t, l, root)[1])
	if err != nil || bytes.Contains(b, []byte("session_message")) || bytes.Count(b, []byte("\n")) != 5 {
		t.Errorf("export without session_message (err %v):\n%s", err, b)
	}
}

// A database missing a required table is an error naming it.
func TestMissingTable(t *testing.T) {
	root := t.TempDir()
	w := newDB(t, root)
	exec(t, w, `DROP TABLE part`)
	if _, err := (sqliteLayout{}).Discover(root); err == nil || !strings.Contains(err.Error(), "no part table") {
		t.Errorf("err = %v", err)
	}
}

// Reading never blocks opencode writing, and never creates, changes or
// checkpoints the WAL (adapter spec §6).
func TestReadOnly(t *testing.T) {
	root := t.TempDir()
	w := newDB(t, root)
	db := filepath.Join(root, dbName)
	l := sqliteLayout{}

	// With opencode running: a write lands while the export's read is open.
	_, err := withDB(db, func(_ context.Context, tx *sql.Tx) ([]byte, error) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM message`).Scan(&n); err != nil {
			return nil, err
		}
		exec(t, w, `INSERT INTO message VALUES ('msg_z', ?, 4000, 4000, '{}')`, parentID)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every WAL reader takes a read mark in the shared memory; the
	// database and the WAL stay as opencode left them.
	files := snapshot(t, root)
	delete(files, dbName+"-shm")
	recs := discover(t, l, root)
	if _, err := source.Content(recs[1]); err != nil {
		t.Fatal(err)
	}
	if v := (Adapter{}).Version(root); v != "1.18.31" {
		t.Errorf("version = %q", v)
	}
	got := snapshot(t, root)
	delete(got, dbName+"-shm")
	if !mapsEqual(got, files) {
		t.Errorf("reading changed the database or its WAL")
	}

	// With opencode closed, its WAL checkpointed and removed: nothing is created.
	w.Close()
	if _, err := os.Stat(db + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("opencode's WAL is still there: %v", err)
	}
	files = snapshot(t, root)
	recs = discover(t, l, root)
	b, err := source.Content(recs[1])
	if err != nil || !bytes.Contains(b, []byte("msg_z")) {
		t.Fatalf("export after close (err %v):\n%s", err, b)
	}
	if got := snapshot(t, root); !mapsEqual(got, files) {
		t.Errorf("reading a closed database created files: %v", got)
	}
}

// snapshot is every file under root with its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	es, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Type().IsRegular() {
			b, _ := os.ReadFile(filepath.Join(root, e.Name()))
			m[e.Name()] = string(b)
		}
	}
	return m
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Exclude reads a Session's directory, and a Child Session's parent.
func TestSqliteStartCwdAndParent(t *testing.T) {
	root := t.TempDir()
	newDB(t, root)
	l := sqliteLayout{}
	recs := discover(t, l, root)
	child, parent := recs[0], recs[1]
	if cwd, _ := l.StartCwd(parent); cwd != "/Users/ted/src/app" {
		t.Errorf("parent cwd = %q", cwd)
	}
	if _, ok := l.Parent(root, parent); ok {
		t.Error("top-level Session has a parent")
	}
	p, ok := l.Parent(root, child)
	if !ok || p.Key != parent.Key {
		t.Fatalf("child's parent = %+v, %v", p, ok)
	}
	if cwd, _ := l.StartCwd(p); cwd != "/Users/ted/src/app" {
		t.Errorf("parent-by-lookup cwd = %q", cwd)
	}
}

// A database elsewhere, from sources.opencode.db, is read there.
func TestConfiguredDB(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	newDB(t, dir)
	a := Adapter{}.WithDB(filepath.Join(dir, dbName))
	if !a.Detect(root) {
		t.Fatal("not detected")
	}
	l := a.Layouts()[1]
	if got := l.WatchPaths(root); !slices.Equal(got, []string{filepath.Join(dir, dbName), filepath.Join(dir, dbName+"-wal")}) {
		t.Errorf("watch paths = %q", got)
	}
	if n := len(discover(t, l, root)); n != 2 {
		t.Errorf("records = %d", n)
	}
}

func writeFile(t *testing.T, root, rel, body string) string {
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

// Legacy files ship under the §2.3 keys; a part's key names its Session.
func TestLegacyKeys(t *testing.T) {
	root := t.TempDir()
	const ses, child, msg, prt = "ses_aaa", "ses_bbb", "msg_111", "prt_111"
	writeFile(t, root, "storage/session/proj1/"+ses+".json", `{"id":"ses_aaa","directory":"/Users/ted/src/app"}`)
	writeFile(t, root, "storage/session/proj1/"+child+".json", `{"id":"ses_bbb","parentID":"ses_aaa"}`)
	writeFile(t, root, "storage/project/proj1.json", `{"worktree":"/Users/ted/src/wt"}`)
	writeFile(t, root, "storage/message/"+ses+"/"+msg+".json", `{}`)
	writeFile(t, root, "storage/part/"+msg+"/"+prt+".json", `{}`)
	writeFile(t, root, "storage/part/msg_gone/prt_222.json", `{}`)
	writeFile(t, root, "storage/session/info/ses_old.json", `{}`)
	writeFile(t, root, "storage/session_diff/ses_aaa.json", `[]`)
	writeFile(t, root, "storage/migration", `2`)
	writeFile(t, root, "storage/part/"+msg+"/notes.txt", `x`)

	a := Adapter{}
	if !a.Detect(root) {
		t.Fatal("not detected")
	}
	l := a.Layouts()[0]
	if !source.Present(l, root) || source.Present(a.Layouts()[1], root) {
		t.Error("want legacy-json present, sqlite absent")
	}
	recs := discover(t, l, root)
	want := []string{
		"json:session/proj1/ses_aaa.json",
		"json:session/proj1/ses_bbb.json",
		"json:message/ses_aaa/msg_111.json",
		"json:part/ses_aaa/msg_111/prt_111.json",
		"json:part/_/msg_gone/prt_222.json",
	}
	if got := keys(recs); !slices.Equal(got, want) {
		t.Errorf("keys = %q\nwant %q", got, want)
	}

	// Claims agrees with Discover, even before any Discover.
	indexes.Lock()
	clear(indexes.m)
	indexes.Unlock()
	for _, r := range recs {
		got, ok := l.Claims(root, r.Path)
		if !ok || got != r {
			t.Errorf("Claims(%s) = %+v, %v; want %+v", r.Path, got, ok, r)
		}
	}

	byKey := map[string]source.Record{}
	for _, r := range recs {
		byKey[r.Key] = r
	}
	main := byKey[want[0]]
	if cwd, _ := l.StartCwd(main); cwd != "/Users/ted/src/app" {
		t.Errorf("cwd = %q", cwd)
	}
	// Without a directory, the project's worktree.
	if cwd, _ := l.StartCwd(byKey[want[1]]); cwd != "/Users/ted/src/wt" {
		t.Errorf("worktree cwd = %q", cwd)
	}
	for _, k := range []string{want[1], want[2], want[3]} {
		if p, ok := l.Parent(root, byKey[k]); !ok || p != main {
			t.Errorf("Parent(%s) = %+v, %v", k, p, ok)
		}
	}
	for _, k := range []string{want[0], want[4]} {
		if p, ok := l.Parent(root, byKey[k]); ok {
			t.Errorf("Parent(%s) = %+v", k, p)
		}
	}

	// Drift: everything is claimed or known-ignored, but the stray file.
	writeFile(t, root, dbName+"-shm", "")
	writeFile(t, root, "opencode-dev.db", "")
	writeFile(t, root, "log/x.log", "")
	res := drift.Scan(a, root)
	if !slices.Equal(res.Unclaimed, []string{"storage/part/msg_111/notes.txt"}) {
		t.Errorf("unclaimed = %q", res.Unclaimed)
	}
	if len(res.Ignored) != 6 {
		t.Errorf("ignored = %+v", res.Ignored)
	}
}

// The database file is claimed; its WAL and shared memory are known-ignored.
func TestDatabaseDrift(t *testing.T) {
	root := t.TempDir()
	newDB(t, root)
	res := drift.Scan(Adapter{}, root)
	if len(res.Unclaimed) != 0 {
		t.Errorf("unclaimed = %q", res.Unclaimed)
	}
	var ignored []string
	for _, p := range res.Ignored {
		ignored = append(ignored, p.Path)
	}
	slices.Sort(ignored)
	if !slices.Equal(ignored, []string{dbName + "-shm", dbName + "-wal"}) {
		t.Errorf("ignored = %q", ignored)
	}
}
