// Package hermes is the Collector adapter for Hermes Agent
// (docs/spec/adapters/hermes.md §2): one Layout, sqlite, exported from
// Hermes's state.db one Session at a time.
package hermes

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/dbexport"
	"github.com/tedkulp/agent-history/protocol"
)

// Adapter is the Hermes Agent Source adapter.
type Adapter struct{}

var _ source.Adapter = Adapter{}

const dbName = "state.db"

// ID is the Source identifier.
func (Adapter) ID() string { return protocol.SourceHermes }

// DefaultRoot is $HERMES_HOME, else ~/.hermes (adapter spec §2.1).
func (Adapter) DefaultRoot(getenv func(string) string, home string) string {
	if d := getenv("HERMES_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".hermes")
}

// Detect is true when the database exists.
func (Adapter) Detect(root string) bool { return dbexport.IsFile(dbPath(root)) }

// Version is empty: state.db records no Hermes version.
func (Adapter) Version(string) string { return "" }

// Layouts is the one sqlite Layout.
func (Adapter) Layouts() []source.Layout { return []source.Layout{sqliteLayout{}} }

// KnownIgnored is the database's WAL and shared memory, read through the
// database (adapter spec §2.3).
func (Adapter) KnownIgnored() []string {
	return []string{dbName + "-wal", dbName + "-shm"}
}

// ScanPaths are the root's state database files. The rest of the root is
// Hermes's own state: config, skills, gateways, other databases.
func (Adapter) ScanPaths(root string) []string {
	return []string{dbPath(root) + "*"}
}

// sqliteLayout is state.db (adapter spec §2.2). Each collected Session is
// one record, keyed by its id, exported with its rows as JSONL.
type sqliteLayout struct{}

var _ source.Database = sqliteLayout{}

func (sqliteLayout) Name() string { return "sqlite" }
func (sqliteLayout) Rank() int    { return 1 } // the only Layout
func (sqliteLayout) IsDatabase()  {}

func dbPath(root string) string { return filepath.Join(root, dbName) }

// WatchPaths are the database and its WAL: a change to either means some
// Session changed.
func (sqliteLayout) WatchPaths(root string) []string {
	p := dbPath(root)
	return []string{p, p + "-wal"}
}

// Claims claims the database file itself, so it isn't unclaimed. It holds
// every record rather than being one: the returned record has no key.
func (sqliteLayout) Claims(root, p string) (source.Record, bool) {
	if p != dbPath(root) {
		return source.Record{}, false
	}
	return source.Record{Path: p}, true
}

// sessionExport is an exported record: one Session of the database at db.
// Cwd and Parent are read when it is discovered, for exclude.
type sessionExport struct {
	db, id, cwd, parent string
}

// Export is the Session's rows as JSONL (adapter spec §2.2). Its errors
// are fs.PathErrors, so that a busy database is retried on the next rescan.
func (s sessionExport) Export() ([]byte, error) {
	b, err := dbexport.Read(s.db, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		if err := checkTables(ctx, tx); err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := dbexport.Rows(ctx, tx, &b, "sessions", `SELECT * FROM sessions WHERE id = ?`, s.id); err != nil {
			return nil, fmt.Errorf("exporting sessions rows: %w", err)
		}
		if err := dbexport.Rows(ctx, tx, &b, "messages", `SELECT * FROM messages WHERE session_id = ? ORDER BY id`, s.id); err != nil {
			return nil, fmt.Errorf("exporting messages rows: %w", err)
		}
		return b.Bytes(), nil
	})
	if err != nil {
		return nil, &fs.PathError{Op: "export " + s.id, Path: s.db, Err: err}
	}
	return b, nil
}

// entryPoints are the Hermes entry points (its sessions.source column)
// whose Sessions are collected. Chat gateways and cron are not (adapter
// spec §2.2).
var entryPoints = []string{"cli", "desktop"}

// subagentEntryPoint marks a Child Session, collected when its parent is.
const subagentEntryPoint = "subagent"

// collectedCTE is every collected Session id: those started from an entry
// point, and, recursively, the Child Sessions (subagent entry point) of a
// collected one.
var collectedCTE = `WITH RECURSIVE collected(id) AS (
  SELECT id FROM sessions WHERE source IN ('` + strings.Join(entryPoints, "','") + `')
  UNION SELECT s.id FROM sessions s JOIN collected c ON s.parent_session_id = c.id WHERE s.source = '` + subagentEntryPoint + `'
)`

// discoverQuery lists every collected Session with its change signal. rows
// counts the Session's rows; fingerprint adds what an in-place rewrite
// changes (compaction flips active and compacted, content is edited),
// each row's share weighted by its id so changes to two rows don't cancel;
// latest is the latest of its times, in unix seconds.
var discoverQuery = collectedCTE + `
SELECT s.id, COALESCE(NULLIF(s.cwd, ''), s.git_repo_root, ''),
  CASE s.source WHEN '` + subagentEntryPoint + `' THEN COALESCE(s.parent_session_id, '') ELSE '' END,
  1 + COALESCE(m.n, 0),
  LENGTH(COALESCE(s.title, '')) + LENGTH(COALESCE(s.cwd, '')) + COALESCE(s.message_count, 0) + COALESCE(m.fp, 0),
  MAX(s.started_at, COALESCE(s.ended_at, 0), COALESCE(s.last_activity_at, 0), COALESCE(m.t, 0))
FROM sessions s JOIN collected USING (id)
LEFT JOIN (
  SELECT session_id, COUNT(*) n, MAX(timestamp) t,
    SUM(id * (LENGTH(COALESCE(content, '')) + LENGTH(COALESCE(tool_calls, '')) + LENGTH(COALESCE(reasoning, ''))
      + LENGTH(role) + LENGTH(COALESCE(tool_call_id, '')) + LENGTH(COALESCE(display_kind, ''))
      + 3 * active + 5 * compacted + 7 * _compressed_summary) + COALESCE(display_order, 0)) fp
  FROM messages GROUP BY session_id
) m ON m.session_id = s.id
ORDER BY s.id`

// Discover lists every collected Session. A record's Stat is its change
// signal: Size combines its row count with a fingerprint of the rows, and
// Mtime is the latest of its times.
func (l sqliteLayout) Discover(root string) ([]source.Record, error) {
	path := dbPath(root)
	var recs []source.Record
	_, err := dbexport.Read(path, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		if err := checkTables(ctx, tx); err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, discoverQuery)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				s      sessionExport
				n, fp  int64
				latest float64
			)
			if err := rows.Scan(&s.id, &s.cwd, &s.parent, &n, &fp, &latest); err != nil {
				return nil, err
			}
			s.db = path
			recs = append(recs, source.Record{
				Key:    s.id,
				Path:   path,
				Export: s,
				Stat:   source.Stat{Size: n<<32 ^ fp, Mtime: unixSeconds(latest)},
			})
		}
		return nil, rows.Err()
	})
	return recs, err
}

// unixSeconds is a Hermes time, float unix seconds, to the microsecond.
func unixSeconds(f float64) time.Time {
	return time.UnixMicro(int64(math.Round(f * 1e6))).UTC()
}

// StartCwd is the Session's cwd, else its git repo root, read when it was
// discovered.
func (sqliteLayout) StartCwd(rec source.Record) (string, error) {
	s, ok := rec.Export.(sessionExport)
	if !ok {
		return "", nil
	}
	return s.cwd, nil
}

// Parent is the parent Session of a Child Session: a subagent Session's
// parent_session_id. Other Sessions with one (compression continuations,
// branches) are their own top-level Sessions (adapter spec §2.5).
func (sqliteLayout) Parent(_ string, rec source.Record) (source.Record, bool) {
	s, ok := rec.Export.(sessionExport)
	if !ok || s.parent == "" {
		return source.Record{}, false
	}
	p := sessionExport{db: s.db, id: s.parent}
	_, err := dbexport.Read(s.db, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		return nil, tx.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(cwd, ''), git_repo_root, ''),
		  CASE source WHEN '`+subagentEntryPoint+`' THEN COALESCE(parent_session_id, '') ELSE '' END FROM sessions WHERE id = ?`,
			p.id).Scan(&p.cwd, &p.parent)
	})
	if err != nil {
		return source.Record{}, false
	}
	return source.Record{Key: p.id, Path: s.db, Export: p}, true
}

// checkTables fails when the database lacks a table the export reads.
func checkTables(ctx context.Context, tx *sql.Tx) error {
	_, err := dbexport.Require(ctx, tx, "hermes", "sessions", "messages")
	return err
}
