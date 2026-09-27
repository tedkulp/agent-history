package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countRecords opens a backup file on its own and counts its Raw records,
// after an integrity check.
func countRecords(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check of %s = %q, %v", path, ok, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM raw_records`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBackupWritesConsistentCopy(t *testing.T) {
	s := openTest(t)
	if _, err := appendChunk(t, s, "m1", "k.jsonl", 0, nil, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "sub", "hub-20260927.db")
	// A .tmp left by a crashed backup doesn't block the next one.
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst+".tmp", []byte("junk"), 0o644)

	if err := s.Backup(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if n := countRecords(t, dst); n != 1 {
		t.Fatalf("backup has %d records, want 1", n)
	}
	if _, err := os.Stat(dst + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".tmp left behind: %v", err)
	}
	// Backing up to the same name again replaces the file.
	if _, err := appendChunk(t, s, "m1", "k2.jsonl", 0, nil, []byte("y\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if n := countRecords(t, dst); n != 2 {
		t.Fatalf("second backup has %d records, want 2", n)
	}
}

func TestIngestContinuesDuringBackup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// Enough incompressible data that VACUUM INTO takes a while.
	blob := make([]byte, 1<<20)
	for i := range 40 {
		rand.Read(blob)
		if _, err := s.Replace(ctx, ReplaceRequest{MachineID: "m1", Source: "src", RecordKey: "big" + string(rune('a'+i)), Data: blob, Compressed: blob}); err != nil {
			t.Fatal(err)
		}
	}

	var running atomic.Bool
	running.Store(true)
	done := make(chan error, 1)
	go func() {
		done <- s.Backup(ctx, filepath.Join(t.TempDir(), "hub.db"))
		running.Store(false)
	}()
	during := 0
	var prefix []byte
	for i := 0; running.Load(); i++ {
		line := []byte("line\n")
		if _, err := appendChunk(t, s, "m1", "live.jsonl", int64(len(prefix)), prefix, line); err != nil {
			t.Fatal(err)
		}
		prefix = append(prefix, line...)
		if running.Load() {
			during++
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if during == 0 {
		t.Fatal("no append committed while the backup ran")
	}
}

// openOld creates dataDir/hub.db at the schema one migration behind this
// binary, holding one Raw record, as an older Hub left it.
func openOld(t *testing.T, dataDir string) (version int) {
	t.Helper()
	migs := mustMigrations(t)
	if len(migs) < 2 {
		t.Skip("needs at least two migrations")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	old := &Store{write: db}
	for _, m := range migs[:len(migs)-1] {
		if err := old.apply(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO machines (id, first_seen_at, last_seen_at) VALUES ('m1', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO raw_records (machine_id, source, record_key) VALUES ('m1', 'src', 'k')`); err != nil {
		t.Fatal(err)
	}
	return len(migs) - 1
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigratingWritesPreMigrateBackupFirst(t *testing.T) {
	ctx := context.Background()
	dataDir, backupDir := t.TempDir(), t.TempDir()
	old := openOld(t, dataDir)

	if _, err := Open(ctx, dataDir, nil); err == nil {
		t.Fatal("migrated without a backup dir")
	}
	s, err := OpenWith(ctx, dataDir, nil, Options{BackupDir: backupDir})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	pre := filepath.Join(backupDir, "pre-migrate-"+strconv.Itoa(old)+".db")
	if n := countRecords(t, pre); n != 1 {
		t.Fatalf("pre-migrate backup has %d records, want 1", n)
	}
	if v := userVersion(t, pre); v != old {
		t.Fatalf("pre-migrate user_version = %d, want %d", v, old)
	}
	if v := userVersion(t, filepath.Join(dataDir, "hub.db")); v != old+1 {
		t.Fatalf("hub.db user_version = %d, want %d", v, old+1)
	}
}

func TestNewDatabaseGetsNoPreMigrateBackup(t *testing.T) {
	backupDir := t.TempDir()
	s, err := OpenWith(context.Background(), t.TempDir(), nil, Options{BackupDir: backupDir})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if entries, _ := os.ReadDir(backupDir); len(entries) != 0 {
		t.Fatalf("backup dir holds %v", entries)
	}
}

func TestFailedPreMigrateBackupStopsMigration(t *testing.T) {
	dataDir := t.TempDir()
	old := openOld(t, dataDir)
	// The backup dir is a file, so the backup can't be written.
	notADir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(notADir, nil, 0o644)
	_, err := OpenWith(context.Background(), dataDir, nil, Options{BackupDir: notADir})
	if err == nil || !strings.Contains(err.Error(), "pre-migrate backup") {
		t.Fatalf("err = %v, want a pre-migrate backup error", err)
	}
	if v := userVersion(t, filepath.Join(dataDir, "hub.db")); v != old {
		t.Fatalf("user_version = %d after a failed backup, want %d", v, old)
	}
}

func TestNewerSchemaMessage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.write.ExecContext(ctx, `PRAGMA user_version = 999`)
	s.Close()
	_, err = OpenWith(ctx, dir, nil, Options{BackupDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "database is from a newer Hub") || !strings.Contains(err.Error(), "restore a pre-migrate backup to roll back") {
		t.Fatalf("err = %v", err)
	}
}

func TestIngestLockedBeyondBusyTimeoutIsErrBusy(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.write.ExecContext(ctx, `PRAGMA busy_timeout = 50`); err != nil {
		t.Fatal(err)
	}
	// A second process holding the write lock, like a `reparse` CLI.
	other, err := sql.Open("sqlite", "file:"+s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)

	start := time.Now()
	_, err = appendChunk(t, s, "m1", "k.jsonl", 0, nil, []byte("x\n"))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("busy_timeout not respected")
	}
}

func TestIngestOnFullDatabaseIsErrDiskFull(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var pages int
	if err := s.write.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// Cap the file at its current size, as a full disk would.
	if _, err := s.write.ExecContext(ctx, `PRAGMA max_page_count = `+strconv.Itoa(pages)); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1<<20)
	rand.Read(blob)
	_, err := s.Replace(ctx, ReplaceRequest{MachineID: "m1", Source: "src", RecordKey: "big", Data: blob, Compressed: blob})
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
}

func TestCheckpointTruncatesWAL(t *testing.T) {
	s := openTest(t)
	if _, err := appendChunk(t, s, "m1", "k.jsonl", 0, nil, []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("WAL is %d bytes after checkpoint", fi.Size())
	}
}
