package ohmypi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

const (
	sessionID = "01a0b085-1516-7000-9081-e596e495299d"
	session   = "-src-app/2026-09-17T17-58-38-183Z_" + sessionID
	mainKey   = session + ".jsonl"
	childKey  = session + "/T3SwitchResearch.jsonl"
	grandKey  = session + "/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch.jsonl"
	childID   = sessionID + "/T3SwitchResearch"
	grandID   = sessionID + "/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch"
)

func TestMapKey(t *testing.T) {
	m := func(id string) parser.Mapping {
		return parser.Mapping{NativeID: id, Role: parser.RoleMain, Layout: "jsonl", LayoutRank: 1}
	}
	for _, c := range []struct {
		key  string
		want parser.Mapping
		ok   bool
	}{
		{mainKey, m(sessionID), true},
		{childKey, m(childID), true},
		{grandKey, m(grandID), true},
		{session + "/SpecReview-2.jsonl", m(sessionID + "/SpecReview-2"), true},
		{mainKey + ".gz", parser.Mapping{}, false},
		{"sessions/" + mainKey, parser.Mapping{}, false},
		{session + "/local/legacy-memories.jsonl", parser.Mapping{}, false},
		{session + "/T3SwitchResearch/Other.jsonl", parser.Mapping{}, false},
		{session + "/T3SwitchResearch.md", parser.Mapping{}, false},
		{"-src-app/2026-09-17T17-58-38-183Z_not-a-sessionID.jsonl", parser.Mapping{}, false},
		{"-src-app/notes.jsonl", parser.Mapping{}, false},
	} {
		got, ok := New().MapKey(c.key)
		if ok != c.ok || got != c.want {
			t.Errorf("MapKey(%q) = %+v, %v; want %+v, %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

func parse(t *testing.T, key string, b []byte) parser.Result {
	t.Helper()
	m, ok := New().MapKey(key)
	if !ok {
		t.Fatalf("MapKey(%q) not ok", key)
	}
	res, err := New().Parse(parser.Input{NativeID: m.NativeID, MainKey: key, Main: b})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// fixture parses a Session file from testdata, filed under its Record key.
func fixture(t *testing.T, key string) (parser.Result, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, key, b), b
}

// summary is one Message as "<id> <role>: <part>, <part>".
func summary(m parser.Message) string {
	var ps []string
	for _, p := range m.Parts {
		s := p.Kind
		switch pl := p.Payload.(type) {
		case parser.TextPayload:
			s += " " + pl.Text
		case parser.ThinkingPayload:
			s += " " + pl.Text
		case parser.MarkerPayload:
			s += " " + pl.Marker + " " + pl.Text
		case parser.ToolCallPayload:
			s += " " + pl.Name + " " + pl.Status
		case parser.AttachmentPayload:
			s += " " + pl.Label
		case parser.UnknownPayload:
			s += " " + pl.SourceType
		case parser.ImagePayload:
			s += " " + pl.MIME
		}
		ps = append(ps, s)
	}
	return m.ID + " " + m.Role + ": " + strings.Join(ps, ", ")
}

func summaries(res parser.Result) []string {
	var out []string
	for _, m := range res.Messages {
		out = append(out, summary(m))
	}
	return out
}

func ms(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

// call is the first tool call with the given name or call id.
func call(t *testing.T, res parser.Result, name string) parser.ToolCallPayload {
	t.Helper()
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if tc, ok := p.Payload.(parser.ToolCallPayload); ok && (tc.Name == name || tc.CallID == name) {
				return tc
			}
		}
	}
	t.Fatalf("no %s call", name)
	return parser.ToolCallPayload{}
}

// The Transcript follows the latest branch; markers, tool calls with their
// results, images and hidden entries map per adapter spec §3.
func TestParseSession(t *testing.T) {
	res, _ := fixture(t, mainKey)

	want := []string{
		"a0000001 assistant: marker model_change openai-codex/gpt-5.6-sol",
		"a0000002 assistant: marker thinking_level high",
		"a0000003 user: marker slash_command skill-prompt: implement",
		"a0000005 assistant: thinking **Reading the spec**, tool_call read ok",
		"a0000009 assistant: tool_call task ok",
		"c0000001 user: text Use the recommended answers",
		"c0000003 assistant: thinking Running it, tool_call eval error, tool_call read ok, image image/png",
		"c0000006 assistant: text Done. [Session persistence truncated large content]",
		"c0000007 user: marker slash_command /clear",
		"c0000008 user: text Look at this",
		"c0000009 user: text and this, attachment image (blob 0123456789ab)",
		"c0000010 assistant: marker compaction Summary of earlier work",
		"c0000011 assistant: marker model_change anthropic/claude-opus-5-5",
		"c0000012 assistant: marker thinking_level medium",
		"c0000014 assistant: unknown future_entry",
		"c0000016 assistant: text All set.",
	}
	if got := summaries(res); !reflect.DeepEqual(got, want) {
		t.Errorf("messages:\n got  %q\n want %q", got, want)
	}

	s := res.Session
	wantS := parser.Session{
		Title:          "Plan the parser",
		StartedAt:      ms("2026-09-17T17:58:38Z"),
		LastActivityAt: ms("2026-09-17T18:03:10Z"),
		Cwd:            "/home/user/src/app",
		SourceVersion:  "schema-3",
	}
	if s != wantS {
		t.Errorf("session:\n got  %+v\n want %+v", s, wantS)
	}

	a := res.Messages[3]
	if a.Model != "gpt-5.6-sol" || a.Provider != "openai-codex" || a.Timestamp != ms("2026-09-17T17:58:50Z") {
		t.Errorf("assistant %q %q %d", a.Model, a.Provider, a.Timestamp)
	}
	if u := *a.Usage; u != (parser.Usage{Input: 7697, Output: 181, CacheRead: 11904, Reasoning: 66}) {
		t.Errorf("usage %+v", u)
	}
	if u := *res.Messages[15].Usage; u != (parser.Usage{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40, Reasoning: 5}) {
		t.Errorf("last usage %+v", u)
	}
	if a.Parts[1].ID != "a0000005.1" {
		t.Errorf("part id %q", a.Parts[1].ID)
	}

	read := call(t, res, "read")
	if read.CallID != "call_read1|fc_1" || *read.Output != "# Spec\n\nline two" || string(read.Input) != `{"i":"Reading the spec","path":"docs/spec.md"}` || read.Diff != nil {
		t.Errorf("read call %+v, output %q", read, *read.Output)
	}
	task := call(t, res, "task")
	if want := []string{sessionID + "/T3SwitchResearch", sessionID + "/T3Other"}; !reflect.DeepEqual(task.ChildSessions, want) {
		t.Errorf("task children %q, want %q", task.ChildSessions, want)
	}
	if eval := call(t, res, "eval"); *eval.Output != "ZeroDivisionError" || eval.ChildSessions == nil || len(eval.ChildSessions) != 0 {
		t.Errorf("eval call %+v", eval)
	}
	// A call's intent is its description, with or without an "i" copy in its
	// arguments; a call without one stores no description.
	if d := read.Description; d != "Reading the spec" {
		t.Errorf("read description %q", d)
	}
	if d := call(t, res, "eval").Description; d != "Checking the error" {
		t.Errorf("eval description %q", d)
	}
	if b, _ := json.Marshal(call(t, res, "call_shot|fc_4")); strings.Contains(string(b), `"description"`) {
		t.Errorf("shot payload %s", b)
	}
	if len(res.Images) != 1 || res.Images[0].MIME != "image/png" || len(res.Images[0].Bytes) == 0 {
		t.Errorf("images %+v", res.Images)
	}

	wantW := []parser.Warning{
		{Kind: parser.WarnBadLine, Count: 1, FirstExcerpt: "{not json"},
		{Kind: parser.WarnUnknownType, SourceType: "future_entry", Count: 1},
		{Kind: parser.WarnOrphan, SourceType: "toolResult", Count: 1},
	}
	if len(res.Warnings) != len(wantW) {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	for i, w := range res.Warnings {
		w.FirstExcerpt = strings.SplitN(w.FirstExcerpt, "\n", 2)[0]
		if i > 0 {
			w.FirstExcerpt = ""
		}
		if w != wantW[i] {
			t.Errorf("warning %d = %+v, want %+v", i, w, wantW[i])
		}
	}

	// Parsing is deterministic.
	again, _ := fixture(t, mainKey)
	if !reflect.DeepEqual(res, again) {
		t.Error("a second parse differs")
	}
}

// omp rewrites the title slot in place; the new title wins (adapter spec §4).
func TestTitleSlotRewritten(t *testing.T) {
	_, b := fixture(t, mainKey)
	first, rest, _ := bytes.Cut(b, []byte("\n"))
	rewritten := bytes.Replace(first, []byte(`"title":"Plan the parser"`), []byte(`"title":"Parser, planned!"`), 1)
	// The slot keeps its width: omp takes the difference from the padding.
	rewritten = bytes.Replace(rewritten, []byte(`"pad":"  `), []byte(`"pad":" `), 1)
	if len(rewritten) != len(first) {
		t.Fatalf("slot width %d, want %d", len(rewritten), len(first))
	}
	res := parse(t, mainKey, append(append(rewritten, '\n'), rest...))
	if res.Session.Title != "Parser, planned!" {
		t.Errorf("title %q", res.Session.Title)
	}

	// An empty slot falls back to the header's title, then to nothing.
	empty := bytes.Replace(first, []byte(`"title":"Plan the parser"`), []byte(`"title":""`), 1)
	res = parse(t, mainKey, append(append(empty, '\n'), rest...))
	if res.Session.Title != "Plan the parser" {
		t.Errorf("title from the header %q", res.Session.Title)
	}
	noHeaderTitle := bytes.Replace(rest, []byte(`,"title":"Plan the parser","titleSource":"auto"`), nil, 1)
	res = parse(t, mainKey, append(append(empty, '\n'), noHeaderTitle...))
	if res.Session.Title != "" {
		t.Errorf("title with none %q", res.Session.Title)
	}
}

// A sub-agent is a Child Session of the file it sits under; its task shows
// once; a task call links forward to nested sub-agents (adapter spec §3.5).
func TestParseChildSessions(t *testing.T) {
	res, _ := fixture(t, childKey)
	want := []string{
		"d0000001 assistant: marker model_change openai-codex/gpt-5.6-sol",
		"d0000003 user: text Complete assignment thoroughly:\n\nFind the switch",
		"d0000004 assistant: tool_call task ok",
		"d0000006 assistant: tool_call yield ok",
		"d0000008 assistant: marker compaction Branch discarded",
	}
	if got := summaries(res); !reflect.DeepEqual(got, want) {
		t.Errorf("messages:\n got  %q\n want %q", got, want)
	}
	if s := res.Session; s.ParentNativeID != sessionID || s.SpawningCallID != "" || s.Title != "explore: T3SwitchResearch" || s.Cwd != "/home/user/src/app" {
		t.Errorf("child session %+v", s)
	}
	if got := call(t, res, "task").ChildSessions; !reflect.DeepEqual(got, []string{grandID}) {
		t.Errorf("nested children %q", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %+v", res.Warnings)
	}

	res, _ = fixture(t, grandKey)
	want = []string{
		"e0000001 user: text Look deeper",
		"e0000002 assistant: text Deeper result",
	}
	if got := summaries(res); !reflect.DeepEqual(got, want) {
		t.Errorf("grandchild messages:\n got  %q\n want %q", got, want)
	}
	if s := res.Session; s.ParentNativeID != childID || s.Title != "scout: T3SwitchResearch.T3ChatSwitch" {
		t.Errorf("grandchild session %+v", s)
	}
}

func lines(entries ...string) []byte { return []byte(strings.Join(entries, "\n") + "\n") }

// v1 has no tree: entries in file order, ids from the native id and line
// position; v2's hookMessage is Raw only (adapter spec §3.2).
func TestSchemaVersions(t *testing.T) {
	v1 := lines(
		`{"type":"session","version":1,"id":"x","timestamp":"2025-01-01T00:00:00Z","cwd":"/src/v1"}`,
		`{"type":"message","timestamp":"2025-01-01T00:00:01Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"message","timestamp":"2025-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m","provider":"p"}}`,
	)
	res := parse(t, mainKey, v1)
	if len(res.Messages) != 2 || res.Session.SourceVersion != "schema-1" || res.Session.Cwd != "/src/v1" {
		t.Fatalf("v1 %+v", res)
	}
	a, b := res.Messages[0].ID, res.Messages[1].ID
	if a == b || a == "" || res.Messages[0].Parts[0].ID != a+".0" {
		t.Errorf("v1 ids %q %q", a, b)
	}
	if again := parse(t, mainKey, v1); again.Messages[0].ID != a {
		t.Error("v1 ids aren't stable")
	}
	// A header with no version and entries with no ids is v1 as well.
	if res := parse(t, mainKey, bytes.Replace(v1, []byte(`"version":1,`), nil, 1)); len(res.Messages) != 2 {
		t.Errorf("versionless v1 %q", summaries(res))
	}

	v2 := lines(
		`{"type":"session","version":2,"id":"x","timestamp":"2025-01-01T00:00:00Z","cwd":"/src/v2"}`,
		`{"type":"message","id":"00000001","parentId":null,"timestamp":"2025-01-01T00:00:01Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"message","id":"00000002","parentId":"00000001","timestamp":"2025-01-01T00:00:02Z","message":{"role":"hookMessage","content":"hook"}}`,
	)
	if got := summaries(parse(t, mainKey, v2)); !reflect.DeepEqual(got, []string{"00000001 user: text hello"}) {
		t.Errorf("v2 %q", got)
	}

	// A newer schema parses as v3.
	v4 := bytes.Replace(v2, []byte(`"version":2`), []byte(`"version":4`), 1)
	if res := parse(t, mainKey, v4); len(res.Messages) != 1 || res.Session.SourceVersion != "schema-4" {
		t.Errorf("v4 %+v", res)
	}
}

// A parentId naming no entry stops the walk with an orphan warning.
func TestMissingParent(t *testing.T) {
	res := parse(t, mainKey, lines(
		`{"type":"session","version":3,"id":"x","timestamp":"2025-01-01T00:00:00Z","cwd":"/src"}`,
		`{"type":"message","id":"00000001","parentId":null,"timestamp":"2025-01-01T00:00:01Z","message":{"role":"user","content":"lost"}}`,
		`{"type":"message","id":"00000003","parentId":"00000002","timestamp":"2025-01-01T00:00:03Z","message":{"role":"user","content":"kept"}}`,
	))
	if got := summaries(res); !reflect.DeepEqual(got, []string{"00000003 user: text kept"}) {
		t.Errorf("messages %q", got)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnOrphan {
		t.Errorf("warnings %+v", res.Warnings)
	}
}

// A file with no header still parses, with a warning.
func TestNoHeader(t *testing.T) {
	res := parse(t, mainKey, lines(`{"type":"message","id":"00000001","parentId":null,"timestamp":"2025-01-01T00:00:01Z","message":{"role":"user","content":"hi"}}`))
	if len(res.Messages) != 1 || len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnMissingField {
		t.Errorf("result %+v", res)
	}
}

func TestTitleCandidateSkipsExtensionMarkersAndReset(t *testing.T) {
	res, _ := fixture(t, mainKey)
	if got := parser.TitleCandidate(res.Messages); got != "Use the recommended answers" {
		t.Errorf("TitleCandidate = %q, want the first prompt past skill-prompt", got)
	}
	for i, m := range res.Messages {
		if m.ID == "c0000007" {
			if got := parser.TitleCandidate(res.Messages[i:]); got != "Look at this" {
				t.Errorf("TitleCandidate after reset = %q, want the prompt past /clear", got)
			}
			return
		}
	}
	t.Fatal("no reset_boundary message")
}
