// Package claudecode is the Collector adapter for Claude Code
// (docs/spec/adapters/claude-code.md §2).
package claudecode

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if recordKey.MatchString(key) {
			out = append(out, source.Record{Key: key, Path: path})
		}
		return nil
	})
	return out, err
}
