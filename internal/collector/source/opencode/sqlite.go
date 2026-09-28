package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // opencode's database, read without CGO

	"github.com/tedkulp/agent-history/internal/collector/source"
)

// sqliteLayout is opencode's database (adapter spec §2.2). Each Session row
// is one record, db:<session id>, exported with its rows as JSONL.
type sqliteLayout struct {
	db string // the configured database; empty is <root>/opencode.db
}

var _ source.Database = sqliteLayout{}

const dbPrefix = "db:"

func (sqliteLayout) Name() string { return "sqlite" }
func (sqliteLayout) Rank() int    { return 2 }
func (sqliteLayout) IsDatabase()  {}

func (l sqliteLayout) path(root string) string { return Adapter{DB: l.db}.dbPath(root) }

// Present is true when the database file exists.
func (l sqliteLayout) Present(root string) bool { return isFile(l.path(root)) }

// WatchPaths are the database and its WAL: a change to either means some
// Session changed.
func (l sqliteLayout) WatchPaths(root string) []string {
	p := l.path(root)
	return []string{p, p + "-wal"}
}

// Claims claims the database file itself, so it isn't unclaimed. It holds
// every record rather than being one: the returned record has no key.
func (l sqliteLayout) Claims(root, p string) (source.Record, bool) {
	if p != l.path(root) {
		return source.Record{}, false
	}
	return source.Record{Path: p}, true
}

// session is an exported record: one Session row of the database at db.
// Dir and Parent are read when it is discovered, for exclude.
type session struct {
	db, id, dir, parent string
}

// Export is the Session's rows as JSONL (adapter spec §2.2). Its errors
// are fs.PathErrors, so that a busy database is retried on the next rescan.
func (s session) Export() ([]byte, error) {
	b, err := withDB(s.db, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		return exportSession(ctx, tx, s.id)
	})
	if err != nil {
		return nil, &fs.PathError{Op: "export " + s.id, Path: s.db, Err: err}
	}
	return b, nil
}

// Discover lists every Session. A record's Stat is its change signal: the
// number of its rows, and the latest time_updated among them.
func (l sqliteLayout) Discover(root string) ([]source.Record, error) {
	path := l.path(root)
	var recs []source.Record
	_, err := withDB(path, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		tables, err := tableSet(ctx, tx)
		if err != nil {
			return nil, err
		}
		q := `SELECT s.id, s.directory, COALESCE(s.parent_id, ''), s.time_updated`
		from := ` FROM session s`
		n := `1`
		for _, t := range []string{"message", "part", "session_message"} {
			if !tables[t] {
				continue
			}
			q += fmt.Sprintf(`, COALESCE(%[1]s.t, 0)`, t)
			n += fmt.Sprintf(` + COALESCE(%[1]s.n, 0)`, t)
			from += fmt.Sprintf(` LEFT JOIN (SELECT session_id, MAX(time_updated) t, COUNT(*) n FROM %[1]s GROUP BY session_id) %[1]s ON %[1]s.session_id = s.id`, t)
		}
		rows, err := tx.QueryContext(ctx, q+`, `+n+from+` ORDER BY s.id`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			var s session
			vals := make([]int64, len(cols)-3)
			dest := []any{&s.id, &s.dir, &s.parent}
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return nil, err
			}
			s.db = path
			latest := int64(0)
			for _, v := range vals[:len(vals)-1] {
				latest = max(latest, v)
			}
			recs = append(recs, source.Record{
				Key:    dbPrefix + s.id,
				Path:   path,
				Export: s,
				Stat:   source.Stat{Size: vals[len(vals)-1], Mtime: time.UnixMilli(latest).UTC()},
			})
		}
		return nil, rows.Err()
	})
	return recs, err
}

// StartCwd is the Session's directory, read when it was discovered.
func (sqliteLayout) StartCwd(rec source.Record) (string, error) {
	s, ok := rec.Export.(session)
	if !ok {
		return "", nil
	}
	return s.dir, nil
}

// Parent is the parent Session of a Child Session.
func (sqliteLayout) Parent(_ string, rec source.Record) (source.Record, bool) {
	s, ok := rec.Export.(session)
	if !ok || s.parent == "" {
		return source.Record{}, false
	}
	p := session{db: s.db, id: s.parent}
	_, err := withDB(s.db, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		return nil, tx.QueryRowContext(ctx, `SELECT directory, COALESCE(parent_id, '') FROM session WHERE id = ?`, p.id).Scan(&p.dir, &p.parent)
	})
	if err != nil {
		return source.Record{}, false
	}
	return source.Record{Key: dbPrefix + p.id, Path: s.db, Export: p}, true
}

// exportTables are the tables exported for a Session, with their row
// order. session_message is skipped when the database has none.
var exportTables = []struct {
	name, where, order string
}{
	{"session", "id", ""},
	{"message", "session_id", "time_created, id"},
	{"part", "session_id", "id"},
	{"session_message", "session_id", "seq"},
}

// exportSession writes each of the Session's rows as a line
// {"table":…,"row":{…}}, every column in table order.
func exportSession(ctx context.Context, tx *sql.Tx, id string) ([]byte, error) {
	tables, err := tableSet(ctx, tx)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, t := range exportTables {
		if !tables[t.name] {
			continue
		}
		q := fmt.Sprintf(`SELECT * FROM %s WHERE %s = ?`, t.name, t.where)
		if t.order != "" {
			q += ` ORDER BY ` + t.order
		}
		if err := exportRows(ctx, tx, &b, t.name, q, id); err != nil {
			return nil, fmt.Errorf("exporting %s rows: %w", t.name, err)
		}
	}
	return b.Bytes(), nil
}

func exportRows(ctx context.Context, tx *sql.Tx, b *bytes.Buffer, table, q, id string) error {
	rows, err := tx.QueryContext(ctx, q, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		b.WriteString(`{"table":`)
		writeJSON(b, table)
		b.WriteString(`,"row":{`)
		for i, c := range cols {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, c)
			b.WriteByte(':')
			switch v := vals[i].(type) {
			case []byte:
				b.WriteString(`{"$base64":`)
				writeJSON(b, base64.StdEncoding.EncodeToString(v))
				b.WriteByte('}')
			default:
				writeJSON(b, v)
			}
		}
		b.WriteString("}}\n")
	}
	return rows.Err()
}

// writeJSON writes v as JSON without HTML escaping, so text reads as stored.
func writeJSON(b *bytes.Buffer, v any) {
	e := json.NewEncoder(b)
	e.SetEscapeHTML(false)
	e.Encode(v)
	b.Truncate(b.Len() - 1) // Encode's newline
}

// requiredTables must exist for the database to be read.
var requiredTables = []string{"session", "message", "part"}

// tableSet is the database's tables, checked for the required ones.
func tableSet(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		set[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, t := range requiredTables {
		if !set[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("opencode database has no %s table", strings.Join(missing, ", "))
	}
	return set, nil
}

// dbVersion is session.version of the most recently updated Session, else empty.
func dbVersion(path string) string {
	var v string
	_, err := withDB(path, func(ctx context.Context, tx *sql.Tx) ([]byte, error) {
		return nil, tx.QueryRowContext(ctx, `SELECT version FROM session ORDER BY time_updated DESC LIMIT 1`).Scan(&v)
	})
	if err != nil {
		return ""
	}
	return v
}

// queryTimeout bounds one read of the database, busy waits included.
const queryTimeout = time.Minute

// withDB runs fn in one read transaction on the database at path, opened
// read-only so that opencode is never blocked and its WAL is never created
// or checkpointed (adapter spec §2.2). A read-only connection still creates
// a missing WAL and shared-memory file, so when they are missing, which
// means no one has the database open for writing, it is read as immutable.
func withDB[T any](path string, fn func(context.Context, *sql.Tx) (T, error)) (T, error) {
	var zero T
	if !isFile(path) {
		return zero, fs.ErrNotExist
	}
	q := "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	if !isFile(path+"-wal") || !isFile(path+"-shm") {
		q = "immutable=1&_pragma=query_only(1)"
	}
	u := url.URL{Path: filepath.ToSlash(path)}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?"+q)
	if err != nil {
		return zero, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	return fn(ctx, tx)
}
