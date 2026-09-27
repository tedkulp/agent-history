// Package drift finds the files under a Source's root that the Collector
// doesn't understand (collector.md §4.7): unclaimed paths, which no Layout
// claims, and known-ignored paths, which the adapter recognizes but doesn't read.
package drift

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/status"
)

// Result is one scan of a Source. Paths are relative to the root.
type Result struct {
	// Unclaimed is sorted by path.
	Unclaimed []string
	// Ignored is sorted newest first.
	Ignored []status.IgnoredPath
}

// Scan checks every regular file under a's ScanPaths against each Layout's
// Claims and a's KnownIgnored globs. Parts of the tree that can't be read
// are skipped: discovery reports those errors.
func Scan(a source.Adapter, root string) Result {
	var r Result
	seen := map[string]bool{}
	visit := func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || seen[path] {
			return nil
		}
		seen[path] = true
		rel, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, l := range a.Layouts() {
			if _, ok := l.Claims(root, path); ok {
				return nil
			}
		}
		for _, g := range a.KnownIgnored() {
			if exclude.Match(g, rel) {
				fi, err := d.Info()
				if err == nil {
					r.Ignored = append(r.Ignored, status.IgnoredPath{Path: rel, Modified: fi.ModTime()})
				}
				return nil
			}
		}
		r.Unclaimed = append(r.Unclaimed, rel)
		return nil
	}
	for _, p := range a.ScanPaths(root) {
		matches, _ := filepath.Glob(p)
		for _, m := range matches {
			filepath.WalkDir(m, visit)
		}
	}
	slices.Sort(r.Unclaimed)
	slices.SortFunc(r.Ignored, func(x, y status.IgnoredPath) int {
		if c := y.Modified.Compare(x.Modified); c != 0 {
			return c
		}
		return strings.Compare(x.Path, y.Path)
	})
	return r
}

// Report fills in s's unclaimed and known-ignored counts and paths.
func (r Result) Report(s *status.Source) {
	s.Unclaimed = len(r.Unclaimed)
	s.UnclaimedPaths = r.Unclaimed[:min(len(r.Unclaimed), status.MaxPaths)]
	s.Ignored = len(r.Ignored)
	s.IgnoredPaths = r.Ignored[:min(len(r.Ignored), status.MaxPaths)]
}
