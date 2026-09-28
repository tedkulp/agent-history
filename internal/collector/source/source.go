// Package source defines the Collector-side Source adapter interface
// (collector.md §2.6). Each Source lives in its own sub-package.
package source

import (
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Adapter finds one Source's Raw records on this Machine.
type Adapter interface {
	// ID is the Source identifier (protocol.md §3.1).
	ID() string
	// DefaultRoot derives the root from an environment lookup and the home directory.
	DefaultRoot(getenv func(string) string, home string) string
	// Detect reports whether the Source is present under root.
	Detect(root string) bool
	// Version is the Source's version if it can be read cheaply, else empty.
	Version(root string) string
	// Layouts is the ranked list of ways the Source stores history.
	Layouts() []Layout
	// KnownIgnored are path globs, relative to the root, that are recognized
	// but deliberately not read. `**` matches any number of segments.
	KnownIgnored() []string
	// ScanPaths are the directories, or root-level file globs, checked for
	// paths no Layout claims (collector.md §4.7).
	ScanPaths(root string) []string
}

// Layout is one way a Source stores its history on disk.
type Layout interface {
	Name() string
	// Rank: higher wins when one Session appears in two Layouts.
	Rank() int
	// Discover lists the Layout's Raw records under root.
	Discover(root string) ([]Record, error)
	// WatchPaths are the directories to watch with fsnotify. Each is
	// watched recursively, with new subdirectories added as they appear.
	WatchPaths(root string) []string
	// Claims returns the Raw record held at path, an absolute path under
	// root, when this Layout claims it.
	Claims(root, path string) (Record, bool)
	// StartCwd is the starting cwd of a Session's record, read cheaply from
	// metadata, or empty while it can't be read yet. Used only for exclude.
	StartCwd(rec Record) (string, error)
	// Parent is the record rec belongs to: a Child Session's parent
	// Session, or the Session an attachment belongs to. Used only for exclude.
	Parent(root string, rec Record) (Record, bool)
}

// Optional is implemented by a Layout that may be absent while its Source
// is detected, such as opencode's legacy-json tree. An absent Layout isn't
// reported or discovered.
type Optional interface {
	Present(root string) bool
}

// Present reports whether l is present under root: always, unless l is Optional.
func Present(l Layout, root string) bool {
	o, ok := l.(Optional)
	return !ok || o.Present(root)
}

// Database is implemented by a Layout whose records are rows in one
// database rather than files (opencode's sqlite, collector.md §4.5). Its
// WatchPaths name files, watched through their directory; a change to any
// of them marks the whole database changed, and Discover then finds the
// records that changed by their Stat. Its records are exports: Export is set.
type Database interface {
	Layout
	IsDatabase()
}

// DBAdapter is implemented by an adapter whose database can live outside
// its root (collector.md §2.3, sources.<id>.db).
type DBAdapter interface {
	Adapter
	// DefaultDB is the database path init writes to config, read from the
	// shell's environment, or empty when the default applies.
	DefaultDB(getenv func(string) string, root string) string
	// WithDB is the adapter reading the database at path.
	WithDB(path string) Adapter
}

// Record is one discovered Raw record.
type Record struct {
	// Key is the Record key, relative to the Source root.
	Key string
	// Path is the absolute path of the file holding the record's content.
	// For an export it is the database file.
	Path string
	// Export, when set, makes the record's content instead of reading Path:
	// a database export, which is JSONL content always sent as a replace
	// (protocol.md §3.2, §4.2). Its dynamic type must be comparable.
	Export Exporter
	// Stat is an export's change signal, taken when it was discovered.
	Stat Stat
}

// Exporter makes an exported record's content.
type Exporter interface {
	Export() ([]byte, error)
}

// Stat is a record's change signal: its file's size and mtime, or what
// the Layout reports for an export (for a database, its row count and
// latest time_updated).
type Stat struct {
	Size  int64
	Mtime time.Time
}

// StatOf is rec's current change signal. For a file it stats Path.
func StatOf(rec Record) (Stat, error) {
	if rec.Export != nil {
		return rec.Stat, nil
	}
	fi, err := os.Stat(rec.Path)
	if err != nil {
		return Stat{}, err
	}
	return Stat{fi.Size(), fi.ModTime()}, nil
}

// Content is rec's content: its export, or its file read decompressed.
func Content(rec Record) ([]byte, error) {
	if rec.Export != nil {
		return rec.Export.Export()
	}
	return ReadFile(rec.Path)
}

// Open opens a record's file for reading its content. A `.zst` or `.gz` file
// is read decompressed (collector.md §2.6).
func Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		z, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, &fs.PathError{Op: "read", Path: path, Err: err}
		}
		return gzipFile{z, f}, nil
	}
	if !strings.HasSuffix(path, ".zst") {
		return f, nil
	}
	d, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return zstdFile{d, f}, nil
}

// zstdFile is a decompressing reader that closes its file.
type zstdFile struct {
	d *zstd.Decoder
	f *os.File
}

func (z zstdFile) Read(p []byte) (int, error) { return z.d.Read(p) }

func (z zstdFile) Close() error {
	z.d.Close()
	return z.f.Close()
}

// gzipFile is a decompressing reader that closes its file.
type gzipFile struct {
	z *gzip.Reader
	f *os.File
}

func (g gzipFile) Read(p []byte) (int, error) { return g.z.Read(p) }

func (g gzipFile) Close() error {
	err := g.z.Close()
	if ferr := g.f.Close(); err == nil {
		err = ferr
	}
	return err
}

// ReadFile reads a record's whole content, decompressed like Open.
func ReadFile(path string) ([]byte, error) {
	r, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: path, Err: err}
	}
	return b, nil
}
