// Package source defines the Collector-side Source adapter interface
// (collector.md §2.6). Each Source lives in its own sub-package.
package source

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
