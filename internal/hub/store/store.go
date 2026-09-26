// Package store is the Hub's SQLite storage: Machines and Raw records
// (hub.md §3). It owns the one writer connection and the reader pool.
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

	"github.com/tedkulp/agent-history/protocol"
)

// readerPoolSize is the number of reader connections (hub.md §3.1).
const readerPoolSize = 4

// Store is the Hub database.
type Store struct {
	write *sql.DB // exactly one connection: every write goes through it
	read  *sql.DB
	log   *slog.Logger
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

// Open opens (creating if needed) dataDir/hub.db and applies pending migrations.
func Open(ctx context.Context, dataDir string) (*Store, error) {
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

	s := &Store{write: write, read: read, log: slog.Default()}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes both connection pools.
func (s *Store) Close() error {
	return errors.Join(s.read.Close(), s.write.Close())
}

func now() int64 { return time.Now().UnixMilli() }

// UpsertMachine registers the Machine or replaces its metadata (hub.md §3.2).
func (s *Store) UpsertMachine(ctx context.Context, id string, info protocol.MachineInfo) error {
	sources, err := json.Marshal(info.Sources)
	if err != nil {
		return err
	}
	t := now()
	_, err = s.write.ExecContext(ctx, `
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
	return err
}

// TouchMachine registers an unknown Machine with only its id, or bumps
// last_seen_at of a known one (protocol.md §2.2).
func (s *Store) TouchMachine(ctx context.Context, id string) error {
	return touchMachine(ctx, s.write, id)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func touchMachine(ctx context.Context, db execer, id string) error {
	t := now()
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

// Append extends a record's current version (protocol.md §4.4), creating the
// record if needed. It returns *ConflictError when Offset or PrefixSha256 do
// not match the current version.
func (s *Store) Append(ctx context.Context, req AppendRequest) (protocol.RecordState, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return protocol.RecordState{}, err
	}
	defer tx.Rollback()

	if err := touchMachine(ctx, tx, req.MachineID); err != nil {
		return protocol.RecordState{}, err
	}

	var (
		recordID  int64
		versionID int64
		version   int64
		length    int64
		sum       = protocol.EmptySha256
		state     []byte
	)
	err = tx.QueryRowContext(ctx, `
		SELECT r.id, v.id, v.version, v.length, v.sha256, v.sha256_state
		FROM raw_records r
		JOIN raw_record_versions v ON v.record_id = r.id AND v.is_current = 1
		WHERE r.machine_id = ? AND r.source = ? AND r.record_key = ?`,
		req.MachineID, req.Source, req.RecordKey).Scan(&recordID, &versionID, &version, &length, &sum, &state)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return protocol.RecordState{}, err
	}

	if req.Offset != length || req.PrefixSha256 != sum {
		return protocol.RecordState{}, &ConflictError{Length: length, Sha256: sum}
	}

	h := sha256.New()
	if exists {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return protocol.RecordState{}, fmt.Errorf("sha256 state of record %d: %w", recordID, err)
		}
	}
	h.Write(req.Data)
	newState, newSum, err := marshalHash(h)
	if err != nil {
		return protocol.RecordState{}, err
	}
	newLength := length + int64(len(req.Data))

	if !exists {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO raw_records (machine_id, source, record_key) VALUES (?, ?, ?)`,
			req.MachineID, req.Source, req.RecordKey)
		if err != nil {
			return protocol.RecordState{}, err
		}
		if recordID, err = res.LastInsertId(); err != nil {
			return protocol.RecordState{}, err
		}
		version = 1
		res, err = tx.ExecContext(ctx, `
			INSERT INTO raw_record_versions (record_id, version, is_current, length, sha256, sha256_state, created_at)
			VALUES (?, 1, 1, ?, ?, ?, ?)`, recordID, newLength, newSum, newState, now())
		if err != nil {
			return protocol.RecordState{}, err
		}
		if versionID, err = res.LastInsertId(); err != nil {
			return protocol.RecordState{}, err
		}
	} else if len(req.Data) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE raw_record_versions SET length = ?, sha256 = ?, sha256_state = ? WHERE id = ?`,
			newLength, newSum, newState, versionID); err != nil {
			return protocol.RecordState{}, err
		}
	}

	if len(req.Data) > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO raw_chunks (version_id, offset, bytes) VALUES (?, ?, ?)`,
			versionID, req.Offset, req.Compressed); err != nil {
			return protocol.RecordState{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return protocol.RecordState{}, err
	}
	return protocol.RecordState{Length: newLength, Sha256: newSum, Version: version}, nil
}

func marshalHash(h hash.Hash) (state []byte, sum string, err error) {
	state, err = h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	return state, hex.EncodeToString(h.Sum(nil)), nil
}

// CurrentContent returns a record's current version, decompressed and
// concatenated in offset order.
func (s *Store) CurrentContent(ctx context.Context, machineID, source, recordKey string) ([]byte, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT c.bytes
		FROM raw_records r
		JOIN raw_record_versions v ON v.record_id = r.id AND v.is_current = 1
		JOIN raw_chunks c ON c.version_id = v.id
		WHERE r.machine_id = ? AND r.source = ? AND r.record_key = ?
		ORDER BY c.offset`, machineID, source, recordKey)
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
