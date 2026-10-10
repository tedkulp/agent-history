package hermes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// The fixtures are synthetic Sessions in a schema_version 30 state.db,
// exported by the Collector's adapter. parentID compacted its start, then
// rewound a turn; childID is a subagent it delegated to.
const (
	parentID = "20261002_140000_a1b2c3"
	childID  = "20261002_140100_c0ffee"
)

func TestMapKey(t *testing.T) {
	for key, ok := range map[string]bool{
		parentID:          true,
		"abc-1.2":         true,
		"":                false,
		"db:" + parentID:  false,
		"a/" + parentID:   false,
		parentID + ".zst": true,
	} {
		got, gotOK := New().MapKey(key)
		want := parser.Mapping{}
		if ok {
			want = parser.Mapping{NativeID: key, Role: parser.RoleMain, Layout: "sqlite", LayoutRank: 1}
		}
		if gotOK != ok || got != want {
			t.Errorf("MapKey(%q) = %+v, %v", key, got, gotOK)
		}
	}
}

func parse(t *testing.T, id string, main []byte) parser.Result {
	t.Helper()
	in := parser.Input{NativeID: id, MainKey: id, Main: main}
	res, err := New().Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := New().Parse(in)
	if !reflect.DeepEqual(res, again) {
		t.Error("Parse isn't deterministic")
	}
	return res
}

func fixture(t *testing.T, id string) parser.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, id, b)
}

// summary is a Message as role and its Parts' kinds, with text where a
// Part has it, for comparing Transcripts at a glance.
func summary(msgs []parser.Message) []string {
	var out []string
	for _, m := range msgs {
		var parts []string
		for _, p := range m.Parts {
			s := p.Kind
			switch pl := p.Payload.(type) {
			case parser.TextPayload:
				s += ":" + pl.Text
			case parser.ThinkingPayload:
				s += ":" + pl.Text
			case parser.MarkerPayload:
				s += ":" + pl.Marker
			case parser.ToolCallPayload:
				s += ":" + pl.Name + "/" + pl.Status
			}
			parts = append(parts, s)
		}
		out = append(out, m.ID+" "+m.Role+" "+strings.Join(parts, ", "))
	}
	return out
}

// The Transcript holds compacted and active rows in display order, drops
// the rewound row, session_meta and hidden scaffolding, and shows the
// compaction summary as a marker.
func TestParseSession(t *testing.T) {
	res := fixture(t, parentID)
	want := []string{
		"2 user text:Why does login fail?",
		"3 assistant thinking:Check the handler first., tool_call:read_file/ok",
		"6 user marker:compaction",
		"7 user text:Now patch it, image",
		"8 assistant text:Patching it now., tool_call:patch/ok, tool_call:terminal/error, tool_call:delegate_task/ok",
		"15 assistant text:Fixed: the handler now checks the cookie.",
	}
	if got := summary(res.Messages); !reflect.DeepEqual(got, want) {
		t.Errorf("messages =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	s := res.Session
	if s.Title != "Fix the login bug" || s.Cwd != "/Users/ted/src/app" || s.GitBranch != "main" || s.ParentNativeID != "" ||
		s.StartedAt != 1790949600125 || s.LastActivityAt != 1790949720600 {
		t.Errorf("session = %+v", s)
	}
	if m := res.Messages[1]; m.Model != "gpt-5.5" || m.Timestamp != 1790949602000 || m.Parts[0].ID != "3.0" {
		t.Errorf("assistant message = %+v", m)
	}
	if mp := res.Messages[2].Parts[0].Payload.(parser.MarkerPayload); mp.Text != "Summary: the login handler ignores the session cookie." {
		t.Errorf("compaction marker = %+v", mp)
	}
	if len(res.Images) != 1 || res.Images[0].MIME != "image/png" {
		t.Errorf("images = %+v", res.Images)
	}

	read := res.Messages[1].Parts[1].Payload.(parser.ToolCallPayload)
	if read.CallID != "call_a" || string(read.Input) != `{"path":"auth/login.go"}` || *read.Output != `{"content": "func login() {}"}` {
		t.Errorf("read_file call = %+v", read)
	}
	patch := res.Messages[4].Parts[1].Payload.(parser.ToolCallPayload)
	if patch.Diff == nil || *patch.Diff != (parser.Diff{Path: "auth/login.go", Old: "if ok {", New: "if ok && cookie != nil {"}) {
		t.Errorf("patch diff = %+v", patch.Diff)
	}

	// The tool row answering a call that isn't in the Transcript.
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnOrphan || res.Warnings[0].SourceType != "tool" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// A subagent Session is a Child Session of its parent, titled by its
// first prompt (the Hub's rule) since Hermes leaves subagents untitled.
func TestParseChildSession(t *testing.T) {
	res := fixture(t, childID)
	s := res.Session
	if s.ParentNativeID != parentID || s.Title != "" || s.Cwd != "" {
		t.Errorf("session = %+v", s)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnMissingField || res.Warnings[0].SourceType != "cwd" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	if got := parser.TitleCandidate(res.Messages); got != "Review the patch" {
		t.Errorf("title candidate = %q", got)
	}
}

// Only a subagent Session is a Child Session: a compression continuation
// or branch names a parent but stands alone.
func TestParentOnlyForSubagents(t *testing.T) {
	for src, want := range map[string]string{"subagent": "p", "cli": "", "desktop": ""} {
		row, _ := json.Marshal(map[string]any{"table": "sessions", "row": map[string]any{
			"id": "s", "source": src, "parent_session_id": "p", "started_at": 1.0, "cwd": "/x"}})
		if got := parse(t, "s", row).Session.ParentNativeID; got != want {
			t.Errorf("%s Session's parent = %q, want %q", src, got, want)
		}
	}
}

// Rows sort by display_order, then id; a row with none sorts by its id.
// cwd falls back to git_repo_root.
func TestDisplayOrder(t *testing.T) {
	line := func(table string, row map[string]any) string {
		b, _ := json.Marshal(map[string]any{"table": table, "row": row})
		return string(b)
	}
	msg := func(id int, order any, text string) string {
		return line("messages", map[string]any{"id": id, "role": "user", "content": text, "timestamp": 1.0, "active": 1, "compacted": 0, "display_order": order})
	}
	main := strings.Join([]string{
		line("sessions", map[string]any{"id": "s", "started_at": 1.0, "cwd": "", "git_repo_root": "/repo"}),
		msg(1, 3, "third"),
		msg(2, 1, "first"),
		msg(3, 1, "second"),
		msg(4, nil, "fourth"),
		"not json",
	}, "\n")
	res := parse(t, "s", []byte(main))
	want := []string{"2 user text:first", "3 user text:second", "1 user text:third", "4 user text:fourth"}
	if got := summary(res.Messages); !reflect.DeepEqual(got, want) {
		t.Errorf("messages = %q", got)
	}
	if res.Session.Cwd != "/repo" {
		t.Errorf("cwd = %q", res.Session.Cwd)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnBadLine {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// A Session with neither cwd nor git_repo_root goes to "No project". Only
// a desktop Session goes there silently: Hermes Desktop sets cwd only when
// the person picks a workspace.
func TestMissingCwd(t *testing.T) {
	for src, want := range map[string]int{"desktop": 0, "cli": 1, "subagent": 1} {
		row, _ := json.Marshal(map[string]any{"table": "sessions", "row": map[string]any{
			"id": "s", "source": src, "started_at": 1.0, "cwd": nil, "git_repo_root": nil}})
		res := parse(t, "s", row)
		if res.Session.Cwd != "" {
			t.Errorf("%s Session's cwd = %q", src, res.Session.Cwd)
		}
		if len(res.Warnings) != want || want == 1 && (res.Warnings[0].Kind != parser.WarnMissingField || res.Warnings[0].SourceType != "cwd") {
			t.Errorf("%s Session's warnings = %+v", src, res.Warnings)
		}
	}
}
