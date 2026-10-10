// Package dbexport reads a Source's live SQLite database without disturbing
// it, and writes its rows as the table-tagged JSONL a database export ships
// (collector.md §4.5). opencode and Hermes Agent both use it.
package dbexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Source databases, read without CGO
)

// queryTimeout bounds one read of the database, busy waits included.
const queryTimeout = time.Minute

// Read runs fn in one read transaction on the database at path, opened
// read-only so that the Source is never blocked and its WAL is never
// created or checkpointed. A read-only connection still creates a missing
// WAL, so when there is none, which means no one has the database open for
// writing, it is read as immutable. A WAL left without its shared memory
// is read through mode=ro, which recreates only the latter.
func Read[T any](path string, fn func(context.Context, *sql.Tx) (T, error)) (T, error) {
	var zero T
	if !IsFile(path) {
		return zero, fs.ErrNotExist
	}
	q := "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	if !IsFile(path + "-wal") {
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

// Rows writes each row q returns as a line {"table":…,"row":{…}}, every
// column in query order. TEXT is a JSON string, INTEGER and REAL a number,
// NULL null, and BLOB {"$base64": "…"}.
func Rows(ctx context.Context, tx *sql.Tx, b *bytes.Buffer, table, q string, args ...any) error {
	rows, err := tx.QueryContext(ctx, q, args...)
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

// Tables is the database's table names.
func Tables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
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
	return set, rows.Err()
}

// Require is the database's tables, failing when any of required is
// missing. name is the Source's, for the error.
func Require(ctx context.Context, tx *sql.Tx, name string, required ...string) (map[string]bool, error) {
	set, err := Tables(ctx, tx)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, t := range required {
		if !set[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s database has no %s table", name, strings.Join(missing, ", "))
	}
	return set, nil
}

// IsFile reports whether p is a regular file.
func IsFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
