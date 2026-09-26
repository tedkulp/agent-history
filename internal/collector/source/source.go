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
}

// Record is one discovered Raw record.
type Record struct {
	// Key is the Record key, relative to the Source root.
	Key string
	// Path is the absolute path of the file holding the record's content.
	Path string
}
