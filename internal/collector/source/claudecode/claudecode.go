// Package claudecode is the Collector adapter for Claude Code
// (docs/spec/adapters/claude-code.md §2).
package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// Adapter is the claude-code Source adapter.
type Adapter struct{}

var _ source.Adapter = Adapter{}

func (Adapter) ID() string { return protocol.SourceClaudeCode }

func (Adapter) DefaultRoot(getenv func(string) string, home string) string {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

func (Adapter) Detect(root string) bool {
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}

func (Adapter) Version(string) string { return "" }

func (Adapter) Layouts() []source.Layout { return []source.Layout{jsonlLayout{}} }

// jsonlLayout is Claude Code's only Layout. Record keys are the path
// relative to the root, with no Layout prefix.
type jsonlLayout struct{}

func (jsonlLayout) Name() string { return "jsonl" }
func (jsonlLayout) Rank() int    { return 1 }

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// recordKey matches every path the jsonl Layout claims (adapter spec §2.2):
// <project>/<session>.jsonl and its orphaned / superseded copies, and under
// <project>/<session>/: tool-results/**, custom-title.json and subagents/agent-*.
var recordKey = regexp.MustCompile(`^[^/]+/` + uuid + `(` +
	`\.jsonl` +
	`|\.orphaned-[^/]+\.jsonl` +
	`|\.jsonl\.superseded-[^/]+` +
	`|/tool-results/.+` +
	`|/custom-title\.json` +
	`|/subagents/agent-[^/]+\.(jsonl|meta\.json)` +
	`)$`)

func (jsonlLayout) Discover(root string) ([]source.Record, error) {
	var out []source.Record
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if rec, ok := (jsonlLayout{}).Claims(root, path); ok {
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

// WatchPaths is the root: every directory under it is watched (adapter spec §2).
func (jsonlLayout) WatchPaths(root string) []string { return []string{root} }

// Claims maps a path to its record: the Record key is the path relative to root.
func (jsonlLayout) Claims(root, path string) (source.Record, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return source.Record{}, false
	}
	key := filepath.ToSlash(rel)
	if strings.HasPrefix(key, "../") || !recordKey.MatchString(key) {
		return source.Record{}, false
	}
	return source.Record{Key: key, Path: path}, true
}

// sessionPrefix is <project>/<session> at the start of a Record key.
var sessionPrefix = regexp.MustCompile(`^[^/]+/` + uuid)

// Parent maps every record other than <project>/<session>.jsonl to that
// Session's <project>/<session>.jsonl (adapter spec §2.4). Child Session
// files are filed flat under the top-level Session, so they map there too.
func (jsonlLayout) Parent(root string, rec source.Record) (source.Record, bool) {
	prefix := sessionPrefix.FindString(rec.Key)
	if prefix == "" || rec.Key == prefix+".jsonl" {
		return source.Record{}, false
	}
	key := prefix + ".jsonl"
	return source.Record{Key: key, Path: filepath.Join(root, filepath.FromSlash(key))}, true
}

// startCwdLimit is how far into a Session file StartCwd reads.
const startCwdLimit = 64 << 10

// StartCwd is the top-level cwd field of the first line that has one,
// within the first 64 KiB (adapter spec §2.4). A line cut short, by the
// limit or because it is still being written, counts when its cwd value is
// complete: a first prompt with a pasted image easily runs past 64 KiB.
func (jsonlLayout) StartCwd(rec source.Record) (string, error) {
	f, err := os.Open(rec.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(io.LimitReader(f, startCwdLimit))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		if cwd := topLevelCwd(line); cwd != "" {
			return cwd, nil
		}
		if err == io.EOF {
			return "", nil
		}
	}
}

// topLevelCwd reads a JSON object's keys in order up to cwd, so the rest of
// the line may be missing.
func topLevelCwd(line []byte) string {
	if !bytes.Contains(line, []byte(`"cwd"`)) {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return ""
		}
		if k == "cwd" {
			if v, err := dec.Token(); err == nil {
				cwd, _ := v.(string)
				return cwd
			}
			return ""
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return ""
		}
	}
	return ""
}
