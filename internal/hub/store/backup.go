package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Backup writes a consistent copy of the database to dst with VACUUM INTO on
// a reader connection, so ingest keeps going (hub.md §4.8). It writes
// dst+".tmp" first and renames it into place, so dst is never partial.
func (s *Store) Backup(ctx context.Context, dst string) error {
	return vacuumInto(ctx, s.read, dst)
}

func vacuumInto(ctx context.Context, db *sql.DB, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	// VACUUM INTO refuses to overwrite; a .tmp left by a crash is stale.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("backup to %s: %w", dst, err)
	}
	if err := fsyncPath(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return fsyncPath(filepath.Dir(dst))
}

// fsyncPath fsyncs a file or directory: VACUUM INTO doesn't sync its output.
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Checkpoint copies the WAL into hub.db and truncates it, for a clean
// shutdown (hub.md §4.1). It reports an error when readers kept it from
// finishing.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal checkpoint incomplete: %d of %d frames", checkpointed, logFrames)
	}
	return nil
}
