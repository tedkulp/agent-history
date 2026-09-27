// Package store is the Hub's SQLite storage: Machines, Raw records, Sessions,
// Transcripts and the parse queue (hub.md §3). It owns the one writer
// connection and the reader pool.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// readerPoolSize is the number of reader connections (hub.md §3.1).
const readerPoolSize = 4

// Store is the Hub database.
type Store struct {
	path    string  // hub.db
	write   *sql.DB // exactly one connection: every write goes through it
	read    *sql.DB
	log     *slog.Logger
	parsers parser.Registry
	clock   func() time.Time
	wake    chan struct{} // signalled after a commit that enqueued a parse
}

// ConflictError is returned by Append when the offset or prefix hash does not
// match the record's current version. It carries the Hub's state.
type ConflictError struct {
	Length int64
	Sha256 string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("offset mismatch: hub has length %d sha256 %s", e.Length, e.Sha256)
}

// Options are the optional settings of OpenWith.
type Options struct {
	// BackupDir receives pre-migrate-<user_version>.db before migrations run
	// on a database that isn't new (hub.md §4.1). When empty, such a
	// database is refused rather than migrated without a backup.
	BackupDir string
}

// Open is OpenWith without Options, for a database that is new or already
// migrated.
func Open(ctx context.Context, dataDir string, parsers parser.Registry) (*Store, error) {
	return OpenWith(ctx, dataDir, parsers, Options{})
}

// OpenWith opens (creating if needed) dataDir/hub.db and applies pending
// migrations. parsers maps Record keys to Sessions on ingest; a Source with
// no parser leaves its records unattached.
func OpenWith(ctx context.Context, dataDir string, parsers parser.Registry, opts Options) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "hub.db")
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	readDSN := "file:" + path + "?" + q.Encode()
	q.Set("_txlock", "immediate")
	writeDSN := "file:" + path + "?" + q.Encode()

	write, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)
	read, err := sql.Open("sqlite", readDSN)
	if err != nil {
		write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(readerPoolSize)

	s := &Store{path: path, write: write, read: read, log: slog.Default(), parsers: parsers, clock: time.Now, wake: make(chan struct{}, 1)}
	if err := s.migrate(ctx, opts.BackupDir); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes both connection pools.
func (s *Store) Close() error {
	return errors.Join(s.read.Close(), s.write.Close())
}

// Ping checks that the database answers SELECT 1 on a reader connection.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.read.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func (s *Store) now() int64 { return s.clock().UnixMilli() }

// SetClock replaces the clock, for tests.
func (s *Store) SetClock(f func() time.Time) { s.clock = f }

// Wake is signalled after an ingest commit that enqueued a parse.
func (s *Store) Wake() <-chan struct{} { return s.wake }

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// UpsertMachine registers the Machine or replaces its metadata (hub.md §3.2).
// When home_dir changes, it recomputes every Session's Project in the same
// transaction, with no re-parse (hub.md §4.4).
func (s *Store) UpsertMachine(ctx context.Context, id string, info protocol.MachineInfo) error {
	sources, err := json.Marshal(info.Sources)
	if err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldHome sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT home_dir FROM machines WHERE id = ?`, id).Scan(&oldHome)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	t := s.now()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO machines (id, display_name, hostname, os, arch, home_dir, collector_version, sources_json, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			display_name = excluded.display_name,
			hostname = excluded.hostname,
			os = excluded.os,
			arch = excluded.arch,
			home_dir = excluded.home_dir,
			collector_version = excluded.collector_version,
			sources_json = excluded.sources_json,
			last_seen_at = excluded.last_seen_at`,
		id, info.DisplayName, info.Hostname, info.OS, info.Arch, info.HomeDir, info.CollectorVersion, string(sources), t, t)
	if err != nil {
		return err
	}
	if oldHome.String != info.HomeDir {
		if err := reassignProjects(ctx, tx, id, info.HomeDir); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TouchMachine registers an unknown Machine with only its id, or bumps
// last_seen_at of a known one (protocol.md §2.2).
func (s *Store) TouchMachine(ctx context.Context, id string) error {
	return touchMachine(ctx, s.write, id, s.now())
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func touchMachine(ctx context.Context, db execer, id string, t int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO machines (id, first_seen_at, last_seen_at) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET last_seen_at = excluded.last_seen_at`, id, t, t)
	return err
}

// Manifest lists the current version of every Raw record of a Machine,
// optionally limited to one Source.
func (s *Store) Manifest(ctx context.Context, machineID, source string) ([]protocol.ManifestRecord, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT r.source, r.record_key, v.length, v.sha256
		FROM raw_records r
		JOIN raw_record_versions v ON v.record_id = r.id AND v.is_current = 1
		WHERE r.machine_id = ? AND (? = '' OR r.source = ?)
		ORDER BY r.source, r.record_key`, machineID, source, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.ManifestRecord{}
	for rows.Next() {
		var m protocol.ManifestRecord
		if err := rows.Scan(&m.Source, &m.RecordKey, &m.Length, &m.Sha256); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AppendRequest is one append chunk. Data is the decompressed body and
// Compressed the same bytes zstd-compressed, stored as the chunk.
type AppendRequest struct {
	MachineID    string
	Source       string
	RecordKey    string
	Offset       int64
	PrefixSha256 string
	Data         []byte
	Compressed   []byte
}

// ReplaceRequest is the first chunk of a new current version. Data is the
// decompressed body and Compressed the same bytes zstd-compressed.
type ReplaceRequest struct {
	MachineID  string
	Source     string
	RecordKey  string
	Data       []byte
	Compressed []byte
}

// current is a record's current version, read inside an ingest transaction.
// A record that doesn't exist yet has length 0 and the empty-string hash.
type current struct {
	exists    bool
	recordID  int64
	sessionID sql.NullInt64
	role      sql.NullString
	versionID int64
	version   int64
	length    int64
	sum       string
	state     []byte
}

// Append extends a record's current version (protocol.md §4.4), creating the
// record if needed. It returns *ConflictError when Offset or PrefixSha256 do
// not match the current version.
func (s *Store) Append(ctx context.Context, req AppendRequest) (protocol.RecordState, error) {
	return s.ingest(ctx, req.MachineID, req.Source, req.RecordKey, func(tx *sql.Tx, cur *current, t int64) (protocol.RecordState, bool, error) {
		if req.Offset != cur.length || req.PrefixSha256 != cur.sum {
			return protocol.RecordState{}, false, &ConflictError{Length: cur.length, Sha256: cur.sum}
		}
		h := sha256.New()
		if cur.exists {
			if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(cur.state); err != nil {
				return protocol.RecordState{}, false, fmt.Errorf("sha256 state of record %d: %w", cur.recordID, err)
			}
		}
		h.Write(req.Data)
		newState, newSum, err := marshalHash(h)
		if err != nil {
			return protocol.RecordState{}, false, err
		}
		newLength := cur.length + int64(len(req.Data))

		version := cur.version
		versionID := cur.versionID
		if !cur.exists {
			version = 1
			if versionID, err = insertVersion(ctx, tx, cur.recordID, version, newLength, newSum, newState, t); err != nil {
				return protocol.RecordState{}, false, err
			}
		} else if len(req.Data) > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE raw_record_versions SET length = ?, sha256 = ?, sha256_state = ? WHERE id = ?`,
				newLength, newSum, newState, versionID); err != nil {
				return protocol.RecordState{}, false, err
			}
		}
		if err := insertChunk(ctx, tx, versionID, req.Offset, req.Compressed, len(req.Data)); err != nil {
			return protocol.RecordState{}, false, err
		}
		return protocol.RecordState{Length: newLength, Sha256: newSum, Version: version}, len(req.Data) > 0, nil
	})
}

// Replace starts a new current version of a record holding Data, and keeps
// the old one as superseded (protocol.md §3.4, §4.4). A body equal to the
// current version is a no-op that returns the current state, so a retried
// replace is safe. The Collector always sends a replace as the first chunk,
// so a match means that chunk already became a whole current version.
func (s *Store) Replace(ctx context.Context, req ReplaceRequest) (protocol.RecordState, error) {
	return s.ingest(ctx, req.MachineID, req.Source, req.RecordKey, func(tx *sql.Tx, cur *current, t int64) (protocol.RecordState, bool, error) {
		h := sha256.New()
		h.Write(req.Data)
		newState, newSum, err := marshalHash(h)
		if err != nil {
			return protocol.RecordState{}, false, err
		}
		newLength := int64(len(req.Data))
		if cur.exists && cur.length == newLength && cur.sum == newSum {
			return protocol.RecordState{Length: cur.length, Sha256: cur.sum, Version: cur.version}, false, nil
		}
		if cur.exists {
			if _, err := tx.ExecContext(ctx, `
				UPDATE raw_record_versions SET is_current = 0 WHERE id = ?`, cur.versionID); err != nil {
				return protocol.RecordState{}, false, err
			}
		}
		version := cur.version + 1
		versionID, err := insertVersion(ctx, tx, cur.recordID, version, newLength, newSum, newState, t)
		if err != nil {
			return protocol.RecordState{}, false, err
		}
		if err := insertChunk(ctx, tx, versionID, 0, req.Compressed, len(req.Data)); err != nil {
			return protocol.RecordState{}, false, err
		}
		return protocol.RecordState{Length: newLength, Sha256: newSum, Version: version}, true, nil
	})
}

// applyFunc writes one chunk against the record's current version. changed
// reports whether the record's content changed, which live-enqueues its
// Session.
type applyFunc func(tx *sql.Tx, cur *current, t int64) (st protocol.RecordState, changed bool, err error)

// ingest runs one records request in a transaction on the writer
// (protocol.md §4.4): it registers the Machine, loads the record's current
// version (creating and attaching the record row if needed), lets apply
// write the chunk, and live-enqueues the Session when apply reports the
// content changed. A busy or full database comes back as ErrBusy or
// ErrDiskFull.
func (s *Store) ingest(ctx context.Context, machineID, source, recordKey string, apply applyFunc) (_ protocol.RecordState, err error) {
	defer func() { err = classify(err) }()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return protocol.RecordState{}, err
	}
	defer tx.Rollback()

	t := s.now()
	if err := touchMachine(ctx, tx, machineID, t); err != nil {
		return protocol.RecordState{}, err
	}

	cur := current{sum: protocol.EmptySha256}
	err = tx.QueryRowContext(ctx, `
		SELECT r.id, r.session_id, r.role, v.id, v.version, v.length, v.sha256, v.sha256_state
		FROM raw_records r
		JOIN raw_record_versions v ON v.record_id = r.id AND v.is_current = 1
		WHERE r.machine_id = ? AND r.source = ? AND r.record_key = ?`,
		machineID, source, recordKey).Scan(&cur.recordID, &cur.sessionID, &cur.role, &cur.versionID, &cur.version, &cur.length, &cur.sum, &cur.state)
	cur.exists = err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return protocol.RecordState{}, err
	}

	// A record created here is rolled back with the transaction if apply
	// refuses the request (a conflicting append).
	if !cur.exists {
		if err := s.createRecord(ctx, tx, &cur, machineID, source, recordKey); err != nil {
			return protocol.RecordState{}, err
		}
	}

	st, changed, err := apply(tx, &cur, t)
	if err != nil {
		return protocol.RecordState{}, err
	}

	enqueued := changed && cur.sessionID.Valid && (cur.role.String == parser.RoleMain || cur.role.String == parser.RoleAttachment)
	if enqueued {
		if err := liveEnqueue(ctx, tx, cur.sessionID.Int64, t); err != nil {
			return protocol.RecordState{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.RecordState{}, err
	}
	if enqueued {
		s.signal()
	}
	return st, nil
}

// createRecord inserts the raw_records row and attaches it to its Session
// when the Source's parser maps the key (hub.md §4.2). A record that takes
// over as the Session's main is live-enqueued by ingest like any main.
func (s *Store) createRecord(ctx context.Context, tx *sql.Tx, cur *current, machineID, source, recordKey string) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO raw_records (machine_id, source, record_key) VALUES (?, ?, ?)`,
		machineID, source, recordKey)
	if err != nil {
		return err
	}
	if cur.recordID, err = res.LastInsertId(); err != nil {
		return err
	}
	if m, ok := s.parsers.MapKey(source, recordKey); ok {
		id, role, err := s.attachRecord(ctx, tx, cur.recordID, machineID, source, m)
		if err != nil {
			return err
		}
		cur.sessionID = sql.NullInt64{Int64: id, Valid: true}
		cur.role = sql.NullString{String: role, Valid: true}
	}
	return nil
}

func insertVersion(ctx context.Context, tx *sql.Tx, recordID, version, length int64, sum string, state []byte, t int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO raw_record_versions (record_id, version, is_current, length, sha256, sha256_state, created_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)`, recordID, version, length, sum, state, t)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// insertChunk stores one compressed chunk; an empty body stores nothing.
func insertChunk(ctx context.Context, tx *sql.Tx, versionID, offset int64, compressed []byte, n int) error {
	if n == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO raw_chunks (version_id, offset, bytes) VALUES (?, ?, ?)`, versionID, offset, compressed)
	return err
}

func marshalHash(h hash.Hash) (state []byte, sum string, err error) {
	state, err = h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	return state, hex.EncodeToString(h.Sum(nil)), nil
}

// CurrentContent returns a record's current version, decompressed and
// concatenated in offset order. A missing record has empty content.
func (s *Store) CurrentContent(ctx context.Context, machineID, source, recordKey string) ([]byte, error) {
	var id int64
	err := s.read.QueryRowContext(ctx, `
		SELECT id FROM raw_records WHERE machine_id = ? AND source = ? AND record_key = ?`,
		machineID, source, recordKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.recordContent(ctx, id)
}

// recordContent returns the current version of a record by id, decompressed.
func (s *Store) recordContent(ctx context.Context, recordID int64) ([]byte, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT c.bytes
		FROM raw_record_versions v
		JOIN raw_chunks c ON c.version_id = v.id
		WHERE v.record_id = ? AND v.is_current = 1
		ORDER BY c.offset`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var out []byte
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, err
		}
		if out, err = dec.DecodeAll(chunk, out); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}
