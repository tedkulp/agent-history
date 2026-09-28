package runner

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/opencode"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	ocParent = "ses_1ba0a4a04ffeLs3vMw9nTpQx7c"
	ocChild  = "ses_1ba0755e0ffeMq2vLs4cTpNw8x"
	ocOther  = "ses_1ba0511f3ffeNc7tWpQm9sLx2v"
)

// ocDB creates an opencode database under the fixture's root, held open in
// WAL mode as opencode holds it, with a parent Session in cwd and its
// Child Session in /tmp.
func (f *fixture) ocDB(cwd string) *sql.DB {
	f.t.Helper()
	f.write("storage/migration", "2")
	db, err := sql.Open("sqlite", f.path("opencode.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { db.Close() })
	f.ocExec(db, `PRAGMA journal_mode=WAL`)
	f.ocExec(db, `
CREATE TABLE session (id text PRIMARY KEY, parent_id text, directory text NOT NULL, title text NOT NULL,
  version text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
  time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
  time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`)
	f.ocExec(db, `INSERT INTO session VALUES (?, NULL, ?, 'Plan', '1.18.31', 1000, 1000), (?, ?, '/tmp', 'Explore', '1.18.31', 1100, 1100)`,
		ocParent, cwd, ocChild, ocParent)
	f.ocExec(db, `INSERT INTO message VALUES ('msg_1', ?, 1000, 1000, '{"role":"user"}')`, ocParent)
	return db
}

func (f *fixture) ocExec(db *sql.DB, q string, args ...any) {
	f.t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// ocExport is the Session's current export.
func (f *fixture) ocExport(id string) string {
	f.t.Helper()
	l := opencode.Adapter{}.Layouts()[1]
	recs, err := l.Discover(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range recs {
		if r.Key == "db:"+id {
			b, err := source.Content(r)
			if err != nil {
				f.t.Fatal(err)
			}
			return string(b)
		}
	}
	f.t.Fatalf("no Session %s", id)
	return ""
}

// Each Session ships as db:<id>; a new message in a live Session ships as a
// replace; an unchanged Session sends nothing; legacy files ship beside
// (opencode.md §6).
func TestOpencodeDatabase(t *testing.T) {
	f := newFixture(t)
	db := f.ocDB("/Users/ted/src/app")
	f.write("storage/session/proj/ses_legacy1.json", `{"id":"ses_legacy1","directory":"/Users/ted/src/app"}`)
	f.write("storage/message/ses_legacy1/msg_9.json", `{"id":"msg_9"}`)
	f.write("storage/part/msg_9/prt_9.json", `{"id":"prt_9"}`)

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: opencode.Adapter{}, Root: f.root}}
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the Sessions to ship", func() bool {
		return f.hubHolds("db:"+ocParent, f.ocExport(ocParent)) && f.hubHolds("db:"+ocChild, f.ocExport(ocChild)) &&
			f.onHub("json:part/ses_legacy1/msg_9/prt_9.json")
	})
	rep := f.status(cfg.Control)
	if s := rep.Sources[0]; s.ID != protocol.SourceOpencode || s.Records != 5 || s.Unclaimed != 0 ||
		!slices.Equal(s.Layouts, []string{"legacy-json", "sqlite"}) {
		t.Errorf("status source %+v, unclaimed %q", s, s.UnclaimedPaths)
	}

	// A new message in the live Session lands at the end of its export,
	// yet ships as a replace: rows change in place.
	f.modesMu.Lock()
	f.modes = nil
	f.modesMu.Unlock()
	f.ocExec(db, `INSERT INTO message VALUES ('msg_2', ?, 2000, 2000, '{"role":"assistant"}')`, ocParent)
	updated := f.ocExport(ocParent)
	waitFor(t, 5*time.Second, "the new message to ship", func() bool { return f.hubHolds("db:"+ocParent, updated) })
	f.modesMu.Lock()
	modes := f.modes
	f.modesMu.Unlock()
	if !slices.Equal(modes, []string{"db:" + ocParent + " replace"}) {
		t.Errorf("requests = %q, want one replace of the parent", modes)
	}

	// Nothing changed: a write that touches no Session ships nothing.
	time.Sleep(100 * time.Millisecond)
	posts := f.posts.Load()
	f.ocExec(db, `CREATE TABLE todo (id text)`)
	time.Sleep(300 * time.Millisecond)
	if n := f.posts.Load() - posts; n != 0 {
		t.Errorf("an unrelated write sent %d records requests", n)
	}
}

// An excluded Session's Child Sessions stay on the Machine too.
func TestOpencodeExcludeCoversChildSessions(t *testing.T) {
	f := newFixture(t)
	db := f.ocDB("/Users/ted/src/secret/app")
	f.ocExec(db, `INSERT INTO session VALUES (?, NULL, '/Users/ted/src/app', 'Open', '1.18.31', 1200, 1200)`, ocOther)

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: opencode.Adapter{}, Root: f.root}}
	cfg.Exclude = excludeFilter(t)
	f.start(cfg)
	waitFor(t, 5*time.Second, "the open Session to ship", func() bool { return f.onHub("db:" + ocOther) })
	time.Sleep(200 * time.Millisecond)
	for _, id := range []string{ocParent, ocChild} {
		if f.onHub("db:" + id) {
			t.Errorf("excluded Session %s reached the Hub", id)
		}
	}
}
