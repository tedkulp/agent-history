// Package ohmypi is the Collector adapter for oh-my-pi
// (docs/spec/adapters/oh-my-pi.md §2).
package ohmypi

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// Adapter is the oh-my-pi Source adapter.
type Adapter struct{}

var _ source.Adapter = Adapter{}

const (
	sessionsDir = "sessions"
	archiveDir  = "archive/sessions"
)

// ID is the Source identifier.
func (Adapter) ID() string { return protocol.SourceOhMyPi }

// DefaultRoot is the first candidate agent directory holding sessions/,
// in omp's own order (adapter spec §2.1), else omp's default.
func (Adapter) DefaultRoot(getenv func(string) string, home string) string {
	config := filepath.Join(home, ".omp")
	if d := getenv("PI_CONFIG_DIR"); d != "" {
		// omp joins it to the home directory, even when absolute.
		config = filepath.Join(home, d)
	}
	profile := getenv("OMP_PROFILE")
	if profile == "" {
		profile = getenv("PI_PROFILE")
	}
	if profile == "default" {
		profile = ""
	}
	fallback := filepath.Join(config, "agent")

	var candidates []string
	xdg := getenv("XDG_DATA_HOME")
	if profile != "" {
		fallback = filepath.Join(config, "profiles", profile, "agent")
		if xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "omp", "profiles", profile))
		}
	} else {
		if d := getenv("PI_CODING_AGENT_DIR"); d != "" {
			candidates = append(candidates, d)
		}
		if xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "omp"))
		}
	}
	candidates = append(candidates, fallback)
	for _, c := range candidates {
		if isDir(filepath.Join(c, sessionsDir)) {
			return c
		}
	}
	return fallback
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Detect is true when either sessions directory exists (adapter spec §2.1).
func (Adapter) Detect(root string) bool {
	return isDir(filepath.Join(root, sessionsDir)) || isDir(filepath.Join(root, archiveDir))
}

// Version is empty: omp doesn't write its version anywhere cheap to read.
func (Adapter) Version(string) string { return "" }

// Layouts is the one jsonl Layout.
func (Adapter) Layouts() []source.Layout { return []source.Layout{jsonlLayout{}} }

// KnownIgnored is omp's lock files and rewrite backups, and the sub-agent
// outputs, tool logs and caches in artifacts directories (adapter spec §2.2).
func (Adapter) KnownIgnored() []string {
	return []string{
		"**/.*.lock", "**/.*.lock.os", "**/*.jsonl.*.bak",
		"**/*.md", "**/*.json", "**/*.log", "**/local/**", "**/url-search/**",
	}
}

// ScanPaths are the two sessions directories. The rest of the agent
// directory is omp's own state.
func (Adapter) ScanPaths(root string) []string {
	return []string{filepath.Join(root, sessionsDir), filepath.Join(root, archiveDir)}
}

// jsonlLayout is omp's only Layout. Record keys are the path relative to
// sessions/ or archive/sessions/ without .gz, so archiving never changes them.
type jsonlLayout struct{}

func (jsonlLayout) Name() string { return "jsonl" }
func (jsonlLayout) Rank() int    { return 1 }

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// sessionRe matches a key's first two segments: <cwd-dir>/<ts>_<uuid>, then
// either .jsonl or the artifacts directory holding a sub-agent.
var sessionRe = regexp.MustCompile(`^[^/]+/\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_` + uuid + `(\.jsonl$|/)`)

// validKey reports whether key names a Session file: a top-level Session,
// or a sub-agent <id>.jsonl in the artifacts directory of another Session
// file, whose own sub-agents are named <id>.<sub> (adapter spec §2.2).
func validKey(key string) bool {
	m := sessionRe.FindStringSubmatchIndex(key)
	if m == nil {
		return false
	}
	if key[m[2]:m[3]] == ".jsonl" {
		return true
	}
	rest, ok := strings.CutSuffix(key[m[1]:], ".jsonl")
	if !ok || rest == "" {
		return false
	}
	segs := strings.Split(rest, "/")
	for i, s := range segs {
		if s == "" || (i > 0 && !strings.HasPrefix(s, segs[i-1]+".")) {
			return false
		}
	}
	return true
}

// preference ranks copies of one key: live before archived, plain before gzipped.
func preference(root, p string) int {
	n := 0
	if !isUnder(filepath.Join(root, sessionsDir), p) {
		n += 2
	}
	if strings.HasSuffix(p, ".gz") {
		n++
	}
	return n
}

func isUnder(dir, p string) bool {
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

// Discover lists one record per key, holding its preferred copy.
func (l jsonlLayout) Discover(root string) ([]source.Record, error) {
	best := map[string]source.Record{}
	var keys []string
	for _, d := range []string{sessionsDir, archiveDir} {
		dir := filepath.Join(root, d)
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == dir {
					return filepath.SkipDir
				}
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			rec, ok := l.claim(root, p)
			if !ok {
				return nil
			}
			cur, seen := best[rec.Key]
			if !seen {
				keys = append(keys, rec.Key)
			}
			if !seen || preference(root, rec.Path) < preference(root, cur.Path) {
				best[rec.Key] = rec
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(keys)
	out := make([]source.Record, 0, len(keys))
	for _, k := range keys {
		out = append(out, best[k])
	}
	return out, nil
}

func (jsonlLayout) WatchPaths(root string) []string {
	return []string{filepath.Join(root, sessionsDir), filepath.Join(root, archiveDir)}
}

// Claims maps a Session file to its record, which holds the key's
// preferred copy.
func (l jsonlLayout) Claims(root, p string) (source.Record, bool) {
	rec, ok := l.claim(root, p)
	if !ok {
		return rec, false
	}
	if best, found := bestCopy(root, rec.Key); found {
		rec = best
	}
	return rec, true
}

func (jsonlLayout) claim(root, p string) (source.Record, bool) {
	for _, d := range []string{sessionsDir, archiveDir} {
		dir := filepath.Join(root, d)
		if !isUnder(dir, p) {
			continue
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return source.Record{}, false
		}
		key := strings.TrimSuffix(filepath.ToSlash(rel), ".gz")
		if !validKey(key) {
			return source.Record{}, false
		}
		return source.Record{Key: key, Path: p}, true
	}
	return source.Record{}, false
}

// bestCopy finds the preferred existing copy of key.
func bestCopy(root, key string) (source.Record, bool) {
	for _, d := range []string{sessionsDir, archiveDir} {
		for _, suffix := range []string{"", ".gz"} {
			p := filepath.Join(root, d, filepath.FromSlash(key)+suffix)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return source.Record{Key: key, Path: p}, true
			}
		}
	}
	return source.Record{}, false
}

// headerLimit is how much of a Session file StartCwd reads.
const headerLimit = 64 << 10

// StartCwd is the session header's cwd, on the first or second line: the
// first is the title slot in v3 (adapter spec §2.4). It is empty while the
// header isn't complete.
func (jsonlLayout) StartCwd(rec source.Record) (string, error) {
	f, err := source.Open(rec.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(io.LimitReader(f, headerLimit))
	for range 2 {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		var h struct {
			Type string `json:"type"`
			Cwd  string `json:"cwd"`
		}
		if json.Unmarshal(line, &h) == nil && h.Type == "session" {
			return h.Cwd, nil
		}
	}
	return "", nil
}

// Parent is a sub-agent's parent Session file: its key with the last
// segment removed, plus .jsonl (adapter spec §2.4).
func (jsonlLayout) Parent(root string, rec source.Record) (source.Record, bool) {
	dir := path.Dir(rec.Key)
	if strings.Count(dir, "/") < 1 {
		return source.Record{}, false
	}
	return bestCopy(root, dir+".jsonl")
}
