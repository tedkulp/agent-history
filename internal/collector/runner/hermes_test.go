package runner

import (
	"database/sql"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/hermes"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	hmCli     = "20261003_090000_a1b2c3"
	hmChild   = "20261003_090100_c0ffee"
	hmGateway = "20261003_100000_9a8b7c"
	hmCron    = "20261003_110000_1d2e3f"
)

// hmDB creates a Hermes state.db under the fixture's root, held open in
// WAL mode as Hermes holds it: a cli Session in cwd with a subagent child,
// and a gateway and a cron Session.
func (f *fixture) hmDB(cwd string) *sql.DB {
	f.t.Helper()
	schema, err := os.ReadFile("../source/hermes/testdata/schema.sql")
	if err != nil {
		f.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path("state.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { db.Close() })
	f.ocExec(db, `PRAGMA journal_mode=WAL`)
	f.ocExec(db, string(schema))
	f.ocExec(db, `INSERT INTO sessions (id, source, parent_session_id, cwd, started_at) VALUES
	  (?, 'cli', NULL, ?, 1000), (?, 'subagent', ?, NULL, 1010), (?, 'telegram', NULL, NULL, 1100), (?, 'cron', NULL, NULL, 1200)`,
		hmCli, cwd, hmChild, hmCli, hmGateway, hmCron)
	f.ocExec(db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, 'user', 'hi', 1001), (?, 'assistant', 'hello', 1002)`, hmCli, hmCli)
	return db
}

// hmExport is the Session's current export.
func (f *fixture) hmExport(id string) string {
	f.t.Helper()
	recs, err := hermes.Adapter{}.Layouts()[0].Discover(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range recs {
		if r.Key == id {
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

// cli Sessions and their subagents ship, keyed by Session id; gateway and
// cron Sessions don't. Compaction rewrites rows in place, and the rewrite
// ships as a replace (hermes.md §6).
func TestHermesDatabase(t *testing.T) {
	f := newFixture(t)
	db := f.hmDB("/Users/ted/src/app")

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: hermes.Adapter{}, Root: f.root}}
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the Sessions to ship", func() bool {
		return f.hubHolds(hmCli, f.hmExport(hmCli)) && f.hubHolds(hmChild, f.hmExport(hmChild))
	})
	if n := f.records(protocol.SourceHermes); n != 2 {
		t.Errorf("the Hub holds %d Hermes records, want 2", n)
	}
	rep := f.status(cfg.Control)
	if s := rep.Sources[0]; s.ID != protocol.SourceHermes || s.Records != 2 || s.Unclaimed != 0 ||
		!slices.Equal(s.Layouts, []string{"sqlite"}) {
		t.Errorf("status source %+v, unclaimed %q", s, s.UnclaimedPaths)
	}

	// Compaction flips the rows it summarizes and adds no newer time.
	f.modesMu.Lock()
	f.modes = nil
	f.modesMu.Unlock()
	f.ocExec(db, `UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ?`, hmCli)
	updated := f.hmExport(hmCli)
	waitFor(t, 5*time.Second, "the compaction to ship", func() bool { return f.hubHolds(hmCli, updated) })
	f.modesMu.Lock()
	modes := f.modes
	f.modesMu.Unlock()
	if !slices.Equal(modes, []string{hmCli + " replace"}) {
		t.Errorf("requests = %q, want one replace of the cli Session", modes)
	}
}

// An excluded Session's subagent Sessions stay on the Machine too.
func TestHermesExcludeCoversChildSessions(t *testing.T) {
	f := newFixture(t)
	db := f.hmDB("/Users/ted/src/secret/app")
	f.ocExec(db, `INSERT INTO sessions (id, source, cwd, started_at) VALUES ('20261003_120000_0pen00', 'desktop', '/Users/ted/src/app', 1300)`)

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: hermes.Adapter{}, Root: f.root}}
	cfg.Exclude = excludeFilter(t)
	f.start(cfg)
	waitFor(t, 5*time.Second, "the open Session to ship", func() bool { return f.onHub("20261003_120000_0pen00") })
	time.Sleep(200 * time.Millisecond)
	for _, id := range []string{hmCli, hmChild} {
		if f.onHub(id) {
			t.Errorf("excluded Session %s reached the Hub", id)
		}
	}
}
