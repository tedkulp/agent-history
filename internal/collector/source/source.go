// Package source defines the Collector-side Source adapter interface
// (collector.md §2.6). Each Source lives in its own sub-package.
package source

import (
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"strings"

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

// Record is one discovered Raw record.
type Record struct {
	// Key is the Record key, relative to the Source root.
	Key string
	// Path is the absolute path of the file holding the record's content.
	Path string
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
	g.z.Close()
	return g.f.Close()
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
