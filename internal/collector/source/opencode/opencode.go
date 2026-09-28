// Package opencode is the Collector adapter for opencode
// (docs/spec/adapters/opencode.md §2): the sqlite Layout, exported from
// opencode's database, and the legacy-json Layout, a tree of JSON files.
package opencode

import (
	"os"
	"path/filepath"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// Adapter is the opencode Source adapter.
type Adapter struct {
	// DB is the database's absolute path; empty is <root>/opencode.db.
	DB string
}

var _ source.DBAdapter = Adapter{}

const (
	storageDir = "storage"
	dbName     = "opencode.db"
)

// ID is the Source identifier.
func (Adapter) ID() string { return protocol.SourceOpencode }

// DefaultRoot is $XDG_DATA_HOME/opencode, else ~/.local/share/opencode, on
// macOS and Linux alike (adapter spec §2.1).
func (Adapter) DefaultRoot(getenv func(string) string, home string) string {
	if d := getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// DefaultDB is OPENCODE_DB resolved against the root, as opencode does, or
// empty when it isn't set or names an in-memory database.
func (Adapter) DefaultDB(getenv func(string) string, root string) string {
	db := getenv("OPENCODE_DB")
	switch {
	case db == "" || db == ":memory:":
		return ""
	case filepath.IsAbs(db):
		return db
	}
	return filepath.Join(root, db)
}

// WithDB is the adapter reading the database at path.
func (a Adapter) WithDB(path string) source.Adapter {
	a.DB = path
	return a
}

// dbPath is the database's path under root.
func (a Adapter) dbPath(root string) string {
	if a.DB != "" {
		return a.DB
	}
	return filepath.Join(root, dbName)
}

// Detect is true when storage/ is a directory or the database exists.
func (a Adapter) Detect(root string) bool {
	return isDir(filepath.Join(root, storageDir)) || isFile(a.dbPath(root))
}

// Version is the version of the most recently updated Session in the
// database, else empty.
func (a Adapter) Version(root string) string {
	return dbVersion(a.dbPath(root))
}

// Layouts are legacy-json (rank 1) and sqlite (rank 2).
func (a Adapter) Layouts() []source.Layout {
	return []source.Layout{legacyLayout{}, sqliteLayout{db: a.DB}}
}

// KnownIgnored is the legacy tree's non-Session state and its pre-2025
// nested tree, channel databases, and the database's WAL and shared
// memory, read through the database (adapter spec §2.3, §2.4).
func (Adapter) KnownIgnored() []string {
	return []string{
		"storage/migration",
		"storage/project/**",
		"storage/session_diff/**",
		"storage/session/info/**",
		"storage/session/message/**",
		"storage/session/part/**",
		"opencode-*.db",
		"opencode-*.db-wal",
		"opencode-*.db-shm",
		dbName + "-wal",
		dbName + "-shm",
	}
}

// ScanPaths are storage/ and the root's database files. The rest of the
// root is opencode's own state.
func (Adapter) ScanPaths(root string) []string {
	return []string{filepath.Join(root, storageDir), filepath.Join(root, "opencode*.db*")}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
