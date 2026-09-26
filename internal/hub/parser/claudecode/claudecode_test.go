package claudecode

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

const sess = "3d34bfcc-90e7-4fd0-900f-86c047b22433"

func TestMapKey(t *testing.T) {
	p := New()
	cases := []struct {
		key  string
		want parser.Mapping
		ok   bool
	}{
		{"-Users-ted-app/" + sess + ".jsonl", parser.Mapping{NativeID: sess, Role: "main", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + "/tool-results/toolu_1.txt", parser.Mapping{NativeID: sess, Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + "/tool-results/p1/page-1.jpg", parser.Mapping{NativeID: sess, Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + "/custom-title.json", parser.Mapping{NativeID: sess, Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + ".orphaned-1700000000-abc.jsonl", parser.Mapping{NativeID: sess, Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + ".jsonl.superseded-1700000000", parser.Mapping{NativeID: sess, Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + "/subagents/agent-a1b2.jsonl", parser.Mapping{NativeID: sess + "/agent-a1b2", Role: "main", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/" + sess + "/subagents/agent-a1b2.meta.json", parser.Mapping{NativeID: sess + "/agent-a1b2", Role: "attachment", Layout: "jsonl", LayoutRank: 1}, true},
		{"-Users-ted-app/not-a-uuid.jsonl", parser.Mapping{}, false},
		{"-Users-ted-app/not-a-uuid/tool-results/x.txt", parser.Mapping{}, false},
		{sess + ".jsonl", parser.Mapping{}, false},
		{"-Users-ted-app/memory/notes.md", parser.Mapping{}, false},
		{"-Users-ted-app/" + sess + ".txt", parser.Mapping{}, false},
	}
	for _, c := range cases {
		got, ok := p.MapKey(c.key)
		if ok != c.ok || got != c.want {
			t.Errorf("MapKey(%q) = %+v, %v; want %+v, %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

// jsonl joins JSON lines built from maps.
func jsonl(t *testing.T, lines ...map[string]any) []byte {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func userLine(uuid, parent, ts string, content any) map[string]any {
	m := map[string]any{
		"type": "user", "uuid": uuid, "sessionId": sess, "timestamp": ts,
		"cwd": "/Users/ted/src/app", "gitBranch": "main", "version": "2.1.283", "isSidechain": false,
		"message": map[string]any{"role": "user", "content": content},
	}
	if parent == "" {
		m["parentUuid"] = nil
	} else {
		m["parentUuid"] = parent
	}
	return m
}

func asstLine(uuid, parent, ts, msgID string, block map[string]any, usage map[string]any) map[string]any {
	msg := map[string]any{"id": msgID, "role": "assistant", "model": "claude-opus-5-5", "content": []any{block}}
	if usage != nil {
		msg["usage"] = usage
	}
	return map[string]any{
		"type": "assistant", "uuid": uuid, "parentUuid": parent, "sessionId": sess, "timestamp": ts,
		"cwd": "/Users/ted/src/app", "gitBranch": "feature", "version": "2.1.284", "isSidechain": false,
		"message": msg,
	}
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func parse(t *testing.T, main []byte, att map[string][]byte) parser.Result {
	t.Helper()
	res, err := New().Parse(parser.Input{NativeID: sess, MainKey: "-Users-ted-src-app/" + sess + ".jsonl", Main: main, Attachments: att, HomeDir: "/Users/ted"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type flat struct {
	ID, Role string
	Texts    []string
}

func flatten(ms []parser.Message) []flat {
	var out []flat
	for _, m := range ms {
		f := flat{ID: m.ID, Role: m.Role}
		for _, p := range m.Parts {
			f.Texts = append(f.Texts, p.Payload.(parser.TextPayload).Text)
		}
		out = append(out, f)
	}
	return out
}

func TestParseSimpleConversation(t *testing.T) {
	last := userLine("u3", "a4", "2026-09-01T10:01:00.000Z", []any{text("second"), text("prompt")})
	last["gitBranch"], last["version"] = "feature", "2.1.284"
	main := jsonl(t,
		map[string]any{"type": "mode", "mode": "default"},
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "hello <b>there</b>"),
		asstLine("a1", "u1", "2026-09-01T10:00:02.000Z", "msg_1", map[string]any{"type": "thinking", "thinking": "hmm", "signature": "x"}, nil),
		asstLine("a2", "a1", "2026-09-01T10:00:03.000Z", "msg_1", text("Hi!"), nil),
		asstLine("a3", "a2", "2026-09-01T10:00:04.000Z", "msg_1", map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}}, nil),
		userLine("u2", "a3", "2026-09-01T10:00:05.000Z", []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}),
		asstLine("a4", "u2", "2026-09-01T10:00:06.000Z", "msg_1", text("Done."), map[string]any{
			"input_tokens": 3, "output_tokens": 50, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 20,
			"output_tokens_details": map[string]any{"thinking_tokens": 7},
		}),
		last,
	)
	res := parse(t, main, nil)

	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"hello <b>there</b>"}},
		{ID: "a1", Role: "assistant", Texts: []string{"Hi!", "Done."}},
		{ID: "u3", Role: "user", Texts: []string{"second", "prompt"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages:\n got %+v\nwant %+v", got, want)
	}
	a := res.Messages[1]
	if a.Model != "claude-opus-5-5" || a.Provider != "anthropic" {
		t.Errorf("model/provider = %q/%q", a.Model, a.Provider)
	}
	if a.Usage == nil || *a.Usage != (parser.Usage{Input: 3, Output: 50, CacheRead: 100, CacheWrite: 20, Reasoning: 7}) {
		t.Errorf("usage = %+v", a.Usage)
	}
	if a.Timestamp != 1788256802000 {
		t.Errorf("assistant timestamp = %d", a.Timestamp)
	}
	if a.Parts[0].ID != "a1.0" || a.Parts[1].ID != "a1.1" {
		t.Errorf("part ids = %q %q", a.Parts[0].ID, a.Parts[1].ID)
	}

	s := res.Session
	if s.Cwd != "/Users/ted/src/app" || s.GitBranch != "feature" || s.SourceVersion != "2.1.284" {
		t.Errorf("session fields = %+v", s)
	}
	if s.StartedAt != 1788256800000 || s.LastActivityAt != 1788256860000 {
		t.Errorf("times = %d .. %d", s.StartedAt, s.LastActivityAt)
	}
	if s.Title != "" || s.ParentNativeID != "" {
		t.Errorf("title/parent = %q/%q", s.Title, s.ParentNativeID)
	}
}

func TestParseSkipsInjectedAndCommandLines(t *testing.T) {
	meta := userLine("u0", "", "2026-09-01T10:00:00.000Z", "caveat")
	meta["isMeta"] = true
	main := jsonl(t,
		meta,
		userLine("u1", "u0", "2026-09-01T10:00:01.000Z", "<command-name>/clear</command-name>\n<command-args></command-args>"),
		userLine("u2", "u1", "2026-09-01T10:00:02.000Z", "<local-command-stdout></local-command-stdout>"),
		userLine("u2b", "u2", "2026-09-01T10:00:02.500Z", "<command-message>review</command-message>\n<command-name>/review</command-name>"),
		map[string]any{"type": "attachment", "uuid": "at1", "parentUuid": "u2b", "timestamp": "2026-09-01T10:00:03.000Z", "attachment": map[string]any{"type": "reminder"}},
		userLine("u3", "at1", "2026-09-01T10:00:04.000Z", "real prompt"),
	)
	res := parse(t, main, nil)
	want := []flat{{ID: "u3", Role: "user", Texts: []string{"real prompt"}}}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseFollowsNewestBranch(t *testing.T) {
	// After /rewind the file holds two branches from u1; the later leaf wins.
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "start"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("first answer"), nil),
		userLine("u2", "a1", "2026-09-01T10:00:02.000Z", "abandoned prompt"),
		asstLine("a2", "u2", "2026-09-01T10:00:03.000Z", "msg_2", text("abandoned answer"), nil),
		userLine("u3", "a1", "2026-09-01T10:00:04.000Z", "retry prompt"),
		asstLine("a3", "u3", "2026-09-01T10:00:05.000Z", "msg_3", text("new answer"), nil),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"start"}},
		{ID: "a1", Role: "assistant", Texts: []string{"first answer"}},
		{ID: "u3", Role: "user", Texts: []string{"retry prompt"}},
		{ID: "a3", Role: "assistant", Texts: []string{"new answer"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseIgnoresSidechainLeafAndOrphans(t *testing.T) {
	side := asstLine("s1", "u1", "2026-09-01T10:00:09.000Z", "msg_s", text("sidechain"), nil)
	side["isSidechain"] = true
	main := jsonl(t,
		userLine("u1", "missing-parent", "2026-09-01T10:00:00.000Z", "start"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("answer"), nil),
		side,
	)
	main = append(main, []byte("{not json\n")...)
	res := parse(t, main, nil)
	if got := len(res.Messages); got != 2 {
		t.Fatalf("messages = %d", got)
	}
	kinds := map[string]bool{}
	for _, w := range res.Warnings {
		kinds[w.Kind+"/"+w.SourceType] = true
	}
	if !kinds["orphan/parentUuid"] || !kinds["bad_line/"] {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseTitlePrecedence(t *testing.T) {
	base := []map[string]any{
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "prompt"),
	}
	customTitle := map[string]any{"type": "custom-title", "customTitle": "Custom", "sessionId": sess}
	aiTitle := map[string]any{"type": "ai-title", "aiTitle": "AI", "sessionId": sess}
	summary := map[string]any{"type": "summary", "summary": "Summary", "leafUuid": "u1"}
	titleJSON := map[string][]byte{"-Users-ted-src-app/" + sess + "/custom-title.json": []byte(`{"customTitle":"FromFile"}`)}

	cases := []struct {
		name  string
		lines []map[string]any
		att   map[string][]byte
		want  string
	}{
		{"custom-title line wins", []map[string]any{summary, aiTitle, customTitle}, titleJSON, "Custom"},
		{"custom-title.json next", []map[string]any{summary, aiTitle}, titleJSON, "FromFile"},
		{"ai-title next", []map[string]any{summary, aiTitle}, nil, "AI"},
		{"summary next", []map[string]any{summary}, nil, "Summary"},
		{"empty falls back to the Hub", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parse(t, jsonl(t, append(append([]map[string]any{}, base...), c.lines...)...), c.att)
			if res.Session.Title != c.want {
				t.Errorf("title = %q, want %q", res.Session.Title, c.want)
			}
		})
	}
}

func TestParseChildSessionParent(t *testing.T) {
	line := asstLine("a1", "", "2026-09-01T10:00:01.000Z", "msg_1", text("child"), nil)
	line["isSidechain"] = true
	res, err := New().Parse(parser.Input{NativeID: sess + "/agent-a1b2", Main: jsonl(t, line), Attachments: map[string][]byte{
		"-Users-ted-src-app/" + sess + "/subagents/agent-a1b2.meta.json": []byte(`{"agentType":"Explore","description":"Find the parser","toolUseId":"toolu_9"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session.Title != "Find the parser" {
		t.Errorf("title = %q", res.Session.Title)
	}
	if res.Session.ParentNativeID != sess {
		t.Errorf("parent = %q", res.Session.ParentNativeID)
	}
	if len(res.Messages) != 1 {
		t.Errorf("every line of a Child Session file is on its path; messages = %d", len(res.Messages))
	}
}

func TestParseIsDeterministic(t *testing.T) {
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "a"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("b"), nil),
	)
	r1, r2 := parse(t, main, nil), parse(t, main, nil)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("two parses of the same input differ")
	}
}

func TestParseMissingCwd(t *testing.T) {
	l := userLine("u1", "", "2026-09-01T10:00:00.000Z", "a")
	delete(l, "cwd")
	res := parse(t, jsonl(t, l), nil)
	if res.Session.Cwd != "" || len(res.Warnings) != 1 || res.Warnings[0].Kind != "missing_field" || res.Warnings[0].SourceType != "cwd" {
		t.Errorf("cwd = %q, warnings = %+v", res.Session.Cwd, res.Warnings)
	}
}

func TestParseGroupingBreaksOnRealUserLines(t *testing.T) {
	// The same message.id on both sides of a real user prompt is two Messages;
	// across a tool result it is one, with usage from the group's last line.
	meta := userLine("m1", "a2", "2026-09-01T10:00:03.000Z", "injected")
	meta["isMeta"] = true
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{}}, map[string]any{"output_tokens": 1}),
		userLine("r1", "a1", "2026-09-01T10:00:01.500Z", []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}),
		asstLine("a2", "r1", "2026-09-01T10:00:02.000Z", "msg_1", text("one"), nil),
		meta,
		asstLine("a3", "m1", "2026-09-01T10:00:04.000Z", "msg_1", text("two"), map[string]any{"output_tokens": 9}),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"one"}},
		{ID: "a3", Role: "assistant", Texts: []string{"two"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if res.Messages[1].Usage != nil {
		t.Errorf("usage taken from a line other than the group's last: %+v", res.Messages[1].Usage)
	}
	if u := res.Messages[2].Usage; u == nil || u.Output != 9 {
		t.Errorf("usage = %+v", u)
	}
}

func TestParseWarnsOnUndecodableMessage(t *testing.T) {
	bad := asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("x"), nil)
	bad["message"] = "not an object"
	res := parse(t, jsonl(t, userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"), bad), nil)
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != "missing_field" || res.Warnings[0].SourceType != "message" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}
