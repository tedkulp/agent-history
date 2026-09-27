// Package exclude decides which Raw records never leave the Machine because
// their Session's starting cwd matches an `exclude` glob (collector.md §4.6).
package exclude

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/tedkulp/agent-history/internal/collector/source"
)

// Layout is the part of a source.Layout the Filter needs.
type Layout interface {
	StartCwd(rec source.Record) (string, error)
	Parent(root string, rec source.Record) (source.Record, bool)
}

// Filter holds the exclude patterns and the decisions made so far, by
// Source and Record key. Decisions are never revisited: a changed exclude
// takes effect on restart. A nil Filter excludes nothing.
type Filter struct {
	patterns []string

	mu      sync.Mutex
	decided map[string]map[string]bool // Source → Record key → excluded
}

// New validates patterns: path.Match syntax per segment, plus `**` for any
// number of segments.
func New(patterns []string) (*Filter, error) {
	for _, p := range patterns {
		for _, seg := range strings.Split(p, "/") {
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("exclude pattern %q: %w", p, err)
			}
		}
	}
	return &Filter{patterns: patterns, decided: map[string]map[string]bool{}}, nil
}

// Match reports whether pattern matches the whole of p. `**` matches zero or
// more path segments; other segments follow path.Match.
func Match(pattern, p string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// Excluded decides whether rec, found by l under root, is excluded. ok is
// false while that can't be known yet (its Session's cwd isn't readable):
// the record then waits and is asked about again. A record with a Parent
// follows the parent's decision, whatever its own cwd.
func (f *Filter) Excluded(src string, l Layout, root string, rec source.Record) (excluded, ok bool) {
	if f == nil || len(f.patterns) == 0 {
		return false, true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decide(src, l, root, rec)
}

func (f *Filter) decide(src string, l Layout, root string, rec source.Record) (excluded, known bool) {
	if ex, ok := f.decided[src][rec.Key]; ok {
		return ex, true
	}
	var ex bool
	if p, ok := l.Parent(root, rec); ok {
		if ex, known = f.decide(src, l, root, p); !known {
			return false, false
		}
	} else {
		cwd, err := l.StartCwd(rec)
		if err != nil || cwd == "" {
			return false, false
		}
		ex = f.match(cwd)
	}
	if f.decided[src] == nil {
		f.decided[src] = map[string]bool{}
	}
	f.decided[src][rec.Key] = ex
	return ex, true
}

func (f *Filter) match(cwd string) bool {
	for _, p := range f.patterns {
		if Match(p, cwd) {
			return true
		}
	}
	return false
}

// Count is how many of src's records are excluded, for `status`.
func (f *Filter) Count(src string) int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ex := range f.decided[src] {
		if ex {
			n++
		}
	}
	return n
}

// Forget drops decisions for src's records that aren't present on disk.
func (f *Filter) Forget(src string, present map[string]bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.decided[src] {
		if !present[k] {
			delete(f.decided[src], k)
		}
	}
}
