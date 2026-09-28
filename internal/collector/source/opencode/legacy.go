package opencode

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tedkulp/agent-history/internal/collector/source"
)

// legacyLayout is opencode's tree of JSON files under storage/ (adapter
// spec §2.3). Each file is its own record, keyed json:<path under storage/>,
// except that a part's key inserts its Session id.
type legacyLayout struct{}

var _ source.Optional = legacyLayout{}

const (
	jsonPrefix = "json:"
	// noSession stands in for the Session of a part whose message file is missing.
	noSession = "_"
)

func (legacyLayout) Name() string { return "legacy-json" }
func (legacyLayout) Rank() int    { return 1 }

// Present is true when storage/session/ is a directory.
func (legacyLayout) Present(root string) bool {
	return isDir(filepath.Join(root, storageDir, "session"))
}

func (legacyLayout) WatchPaths(root string) []string {
	s := filepath.Join(root, storageDir)
	return []string{filepath.Join(s, "session"), filepath.Join(s, "message"), filepath.Join(s, "part")}
}

// Discover lists every session, message and part file, in key order.
func (l legacyLayout) Discover(root string) ([]source.Record, error) {
	idx, err := buildIndex(root)
	if err != nil {
		return nil, err
	}
	var recs []source.Record
	for _, d := range []string{"session", "message", "part"} {
		dir := filepath.Join(root, storageDir, d)
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
			if rec, ok := claim(root, p, idx.lookup); ok {
				recs = append(recs, rec)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return recs, nil
}

// Claims maps a session, message or part file to its record.
func (legacyLayout) Claims(root, p string) (source.Record, bool) {
	return claim(root, p, func(msg string) (string, bool) { return sessionOf(root, msg) })
}

// claim maps the file at p to its record, finding a part's Session with ses.
func claim(root, p string, ses func(msg string) (string, bool)) (source.Record, bool) {
	rel, err := filepath.Rel(filepath.Join(root, storageDir), p)
	if err != nil {
		return source.Record{}, false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	id := func(s, prefix string) bool { return strings.HasPrefix(s, prefix) && len(s) > len(prefix) }
	file := func(s, prefix string) bool {
		name, ok := strings.CutSuffix(s, ".json")
		return ok && id(name, prefix)
	}
	rec := source.Record{Path: p}
	switch {
	case len(segs) == 3 && segs[0] == "session" && segs[1] != "info" && file(segs[2], "ses_"):
		rec.Key = jsonPrefix + strings.Join(segs, "/")
	case len(segs) == 3 && segs[0] == "message" && id(segs[1], "ses_") && file(segs[2], "msg_"):
		rec.Key = jsonPrefix + strings.Join(segs, "/")
	case len(segs) == 3 && segs[0] == "part" && id(segs[1], "msg_") && file(segs[2], "prt_"):
		s, ok := ses(segs[1])
		if !ok {
			s = noSession
		}
		rec.Key = jsonPrefix + path.Join("part", s, segs[1], segs[2])
	default:
		return source.Record{}, false
	}
	return rec, true
}

// index maps message ids to their Session, from the message/ tree.
type index map[string]string

func (x index) lookup(msg string) (string, bool) {
	s, ok := x[msg]
	return s, ok
}

// buildIndex lists storage/message/<ses>/<msg>.json.
func buildIndex(root string) (index, error) {
	x := index{}
	dir := filepath.Join(root, storageDir, "message")
	sessions, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		msgs, err := os.ReadDir(filepath.Join(dir, s.Name()))
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if id, ok := strings.CutSuffix(m.Name(), ".json"); ok {
				x[id] = s.Name()
			}
		}
	}
	indexes.Lock()
	indexes.m[root] = x
	indexes.Unlock()
	return x, nil
}

// indexes holds the last index built per root, so that Claims (called for
// every file on each drift scan) finds a part's Session without a listing.
var indexes = struct {
	sync.Mutex
	m map[string]index
}{m: map[string]index{}}

// sessionOf finds the Session holding message msg: from the last index,
// else by looking for its file.
func sessionOf(root, msg string) (string, bool) {
	indexes.Lock()
	s, ok := indexes.m[root][msg]
	indexes.Unlock()
	if ok {
		return s, true
	}
	matches, _ := filepath.Glob(filepath.Join(root, storageDir, "message", "ses_*", msg+".json"))
	if len(matches) == 0 {
		return "", false
	}
	return filepath.Base(filepath.Dir(matches[0])), true
}

// StartCwd is a session file's directory, else its project's worktree.
// Messages and parts follow their Session through Parent.
func (legacyLayout) StartCwd(rec source.Record) (string, error) {
	if !strings.HasPrefix(rec.Key, jsonPrefix+"session/") {
		return "", nil
	}
	var s struct {
		Directory string `json:"directory"`
		ProjectID string `json:"projectID"`
	}
	if err := readJSON(rec.Path, &s); err != nil {
		return "", err
	}
	if s.Directory != "" {
		return s.Directory, nil
	}
	project := strings.Split(rec.Key, "/")[1]
	if s.ProjectID != "" {
		project = s.ProjectID
	}
	var p struct {
		Worktree string `json:"worktree"`
	}
	storage := filepath.Dir(filepath.Dir(filepath.Dir(rec.Path)))
	if err := readJSON(filepath.Join(storage, "project", project+".json"), &p); err != nil {
		return "", nil
	}
	return p.Worktree, nil
}

// Parent is a message's or part's session file, or a Child Session's
// parent session file, when it exists.
func (legacyLayout) Parent(root string, rec source.Record) (source.Record, bool) {
	segs := strings.Split(strings.TrimPrefix(rec.Key, jsonPrefix), "/")
	var ses string
	switch segs[0] {
	case "message", "part":
		ses = segs[1]
	case "session":
		var s struct {
			ParentID string `json:"parentID"`
		}
		if readJSON(rec.Path, &s) != nil || s.ParentID == "" {
			return source.Record{}, false
		}
		ses = s.ParentID
	}
	if !strings.HasPrefix(ses, "ses_") {
		return source.Record{}, false
	}
	matches, _ := filepath.Glob(filepath.Join(root, storageDir, "session", "*", ses+".json"))
	for _, m := range matches {
		if rec, ok := claim(root, m, nil); ok {
			return rec, true
		}
	}
	return source.Record{}, false
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
