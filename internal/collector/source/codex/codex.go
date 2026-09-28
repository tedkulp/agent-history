// Package codex is the Collector adapter for Codex
// (docs/spec/adapters/codex.md §2).
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// Adapter is the codex Source adapter.
type Adapter struct{}

var _ source.Adapter = Adapter{}

const (
	sessionsDir = "sessions"
	archivedDir = "archived_sessions"
)

func (Adapter) ID() string { return protocol.SourceCodex }

func (Adapter) DefaultRoot(getenv func(string) string, home string) string {
	if dir := getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

// Detect is true when either history directory exists (adapter spec §2.1).
func (Adapter) Detect(root string) bool {
	for _, d := range []string{sessionsDir, archivedDir} {
		if fi, err := os.Stat(filepath.Join(root, d)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func (Adapter) Version(string) string { return "" }

func (Adapter) Layouts() []source.Layout { return []source.Layout{jsonlLayout{}} }

// threadStore is the SQLite thread store Codex is migrating to, not read in v1.
const threadStore = "thread_history_*.sqlite"

func (Adapter) KnownIgnored() []string {
	return []string{threadStore, threadStore + "-wal", threadStore + "-shm"}
}

// ScanPaths are the history directories plus the thread store, so status
// lists it as known-ignored. The rest of the root is Codex's own state.
func (Adapter) ScanPaths(root string) []string {
	return []string{
		filepath.Join(root, sessionsDir),
		filepath.Join(root, archivedDir),
		filepath.Join(root, threadStore+"*"),
	}
}

// jsonlLayout is Codex's only Layout. Record keys are the rollout file's
// basename without `.zst`, so compressing and archiving never change them.
type jsonlLayout struct{}

func (jsonlLayout) Name() string { return "jsonl" }
func (jsonlLayout) Rank() int    { return 1 }

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// fileName matches a rollout file: rollout-<ts>-<thread>[_<rollout>].jsonl,
// optionally compressed (adapter spec §2.2).
var fileName = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-(` + uuid + `)(_` + uuid + `)?\.jsonl(\.zst)?$`)

// preference ranks copies of one key: uncompressed under sessions/ first,
// then compressed, then the same under archived_sessions/.
func preference(root, path string) int {
	n := 0
	if !isUnder(filepath.Join(root, sessionsDir), path) {
		n += 2
	}
	if strings.HasSuffix(path, ".zst") {
		n++
	}
	return n
}

func isUnder(dir, path string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// Discover lists one record per key, holding its preferred copy.
func (l jsonlLayout) Discover(root string) ([]source.Record, error) {
	best := map[string]source.Record{}
	var keys []string
	for _, d := range []string{sessionsDir, archivedDir} {
		err := filepath.WalkDir(filepath.Join(root, d), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && path == filepath.Join(root, d) {
					return filepath.SkipDir
				}
				return err
			}
			if !e.Type().IsRegular() {
				return nil
			}
			rec, ok := l.claim(root, path)
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
	return []string{filepath.Join(root, sessionsDir), filepath.Join(root, archivedDir)}
}

// Claims maps a rollout file to its record. When an uncompressed copy sits
// next to a compressed one, the record holds the uncompressed copy.
func (l jsonlLayout) Claims(root, path string) (source.Record, bool) {
	rec, ok := l.claim(root, path)
	if ok && strings.HasSuffix(rec.Path, ".zst") {
		plain := strings.TrimSuffix(rec.Path, ".zst")
		if fi, err := os.Stat(plain); err == nil && fi.Mode().IsRegular() {
			rec.Path = plain
		}
	}
	return rec, ok
}

func (jsonlLayout) claim(root, path string) (source.Record, bool) {
	if !isUnder(filepath.Join(root, sessionsDir), path) && !isUnder(filepath.Join(root, archivedDir), path) {
		return source.Record{}, false
	}
	name := filepath.Base(path)
	if !fileName.MatchString(name) {
		return source.Record{}, false
	}
	return source.Record{Key: strings.TrimSuffix(name, ".zst"), Path: path}, true
}

// meta is the part of a rollout's first line, session_meta, that exclude
// reads.
type meta struct {
	Type    string `json:"type"`
	Payload struct {
		Cwd            string `json:"cwd"`
		ParentThreadID string `json:"parent_thread_id"`
	} `json:"payload"`
}

// firstLineLimit is how much of a rollout readMeta reads. session_meta
// carries the base instructions, so it can be long.
const firstLineLimit = 4 << 20

// readMeta reads a rollout's first line. ok is false while that line isn't
// complete yet.
func readMeta(path string) (m meta, ok bool, err error) {
	f, err := source.Open(path)
	if err != nil {
		return m, false, err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(io.LimitReader(f, firstLineLimit), 64<<10).ReadBytes('\n')
	if err == io.EOF {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" {
		return meta{}, true, nil
	}
	return m, true, nil
}

// StartCwd is session_meta's cwd (adapter spec §2.4).
func (jsonlLayout) StartCwd(rec source.Record) (string, error) {
	m, _, err := readMeta(rec.Path)
	return m.Payload.Cwd, err
}

// Parent is, for a continuation file, its thread's main record, and for a
// sub-agent thread, its parent thread's main record (adapter spec §2.4).
func (jsonlLayout) Parent(root string, rec source.Record) (source.Record, bool) {
	a := fileName.FindStringSubmatch(rec.Key)
	if a == nil {
		return source.Record{}, false
	}
	if a[2] != "" {
		return findMain(root, a[1])
	}
	m, _, err := readMeta(rec.Path)
	if err != nil || m.Payload.ParentThreadID == "" {
		return source.Record{}, false
	}
	return findMain(root, m.Payload.ParentThreadID)
}

// findMain finds a thread's main rollout, in its preferred copy.
func findMain(root, thread string) (source.Record, bool) {
	name := "rollout-*-" + thread + ".jsonl"
	var found []string
	for _, pat := range []string{
		filepath.Join(root, sessionsDir, "*", "*", "*", name),
		filepath.Join(root, archivedDir, name),
		filepath.Join(root, archivedDir, "*", "*", "*", name),
	} {
		for _, suffix := range []string{"", ".zst"} {
			m, _ := filepath.Glob(pat + suffix)
			found = append(found, m...)
		}
	}
	var best source.Record
	for _, p := range found {
		rec, ok := (jsonlLayout{}).claim(root, p)
		if !ok {
			continue
		}
		if best.Path == "" || preference(root, p) < preference(root, best.Path) {
			best = rec
		}
	}
	return best, best.Path != ""
}
