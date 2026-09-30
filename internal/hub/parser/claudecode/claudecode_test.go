package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
			switch pl := p.Payload.(type) {
			case parser.TextPayload:
				f.Texts = append(f.Texts, pl.Text)
			case parser.ToolCallPayload:
				f.Texts = append(f.Texts, "[tool "+pl.Name+" "+pl.Status+"]")
			case parser.ImagePayload:
				f.Texts = append(f.Texts, "[image "+pl.MIME+"]")
			case parser.ThinkingPayload:
				f.Texts = append(f.Texts, "[thinking "+pl.Text+"]")
			case parser.MarkerPayload:
				f.Texts = append(f.Texts, "["+pl.Marker+" "+pl.Text+"]")
			case parser.UnknownPayload:
				f.Texts = append(f.Texts, "[unknown "+pl.SourceType+"]")
			case parser.AttachmentPayload:
				f.Texts = append(f.Texts, "[attachment "+pl.Label+"]")
			default:
				f.Texts = append(f.Texts, "["+p.Kind+"]")
			}
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
		{ID: "a1", Role: "assistant", Texts: []string{"[thinking hmm]", "Hi!", "[tool Bash ok]", "Done."}},
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
	if a.Parts[0].ID != "a1.0" || a.Parts[3].ID != "a1.3" {
		t.Errorf("part ids = %q %q", a.Parts[0].ID, a.Parts[3].ID)
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

func TestParseCommandsAndInjectedLines(t *testing.T) {
	meta := userLine("u0", "", "2026-09-01T10:00:00.000Z", "caveat")
	meta["isMeta"] = true
	att := func(uuid, parent string, a map[string]any) map[string]any {
		return map[string]any{"type": "attachment", "uuid": uuid, "parentUuid": parent, "timestamp": "2026-09-01T10:00:03.000Z", "attachment": a}
	}
	main := jsonl(t,
		meta,
		userLine("u1", "u0", "2026-09-01T10:00:01.000Z", "<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>"),
		userLine("u2", "u1", "2026-09-01T10:00:02.000Z", "<local-command-stdout></local-command-stdout>"),
		userLine("u2a", "u2", "2026-09-01T10:00:02.100Z", "<local-command-stderr>oops</local-command-stderr>"),
		userLine("u2c", "u2a", "2026-09-01T10:00:02.200Z", "<local-command-caveat>Caveat</local-command-caveat>"),
		userLine("u2b", "u2c", "2026-09-01T10:00:02.500Z", "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>42</command-args>"),
		att("at1", "u2b", map[string]any{"type": "total_tokens_reminder"}),
		att("at2", "at1", map[string]any{"type": "queued_command", "prompt": "meta prompt", "isMeta": true}),
		att("at3", "at2", map[string]any{"type": "queued_command", "prompt": "queued prompt"}),
		att("at4", "at3", map[string]any{"type": "queued_command", "prompt": []any{text("queued blocks")}}),
		userLine("u3", "at4", "2026-09-01T10:00:04.000Z", "real prompt"),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"[slash_command /clear]"}},
		{ID: "u2b", Role: "user", Texts: []string{"[slash_command /review 42]"}},
		{ID: "at3", Role: "user", Texts: []string{"queued prompt"}},
		{ID: "at4", Role: "user", Texts: []string{"queued blocks"}},
		{ID: "u3", Role: "user", Texts: []string{"real prompt"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseShellCommands(t *testing.T) {
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "<bash-input> git status -sb</bash-input>"),
		userLine("u1o", "u1", "2026-09-01T10:00:00.100Z", "<bash-stdout>## main...origin/main</bash-stdout><bash-stderr></bash-stderr>"),
		userLine("u2", "u1o", "2026-09-01T10:00:01.000Z", "<bash-input>make</bash-input>"),
		userLine("u2o", "u2", "2026-09-01T10:00:01.100Z", "<bash-stdout>main -&gt; main</bash-stdout><bash-stderr>warning: &lt;x&gt; &amp;amp;</bash-stderr>"),
		userLine("u3", "u2o", "2026-09-01T10:00:02.000Z", "<bash-input>true</bash-input>"),
		userLine("u3p", "u3", "2026-09-01T10:00:02.500Z", "between"),
		userLine("u4", "u3p", "2026-09-01T10:00:03.000Z", "<bash-stdout>stray</bash-stdout><bash-stderr></bash-stderr>"),
		userLine("u5", "u4", "2026-09-01T10:00:04.000Z", "<bash-input>false</bash-input>"),
		userLine("u5o", "u5", "2026-09-01T10:00:04.100Z", "<bash-stderr>failed</bash-stderr>"),
		userLine("u6", "u5o", "2026-09-01T10:00:05.000Z", "<bash-input>unclosed"),
		userLine("u7", "u6", "2026-09-01T10:00:06.000Z", "real prompt"),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"[shell_command $ git status -sb]"}},
		{ID: "u2", Role: "user", Texts: []string{"[shell_command $ make]"}},
		{ID: "u3", Role: "user", Texts: []string{"[shell_command $ true]"}},
		{ID: "u3p", Role: "user", Texts: []string{"between"}},
		{ID: "u5", Role: "user", Texts: []string{"[shell_command $ false]"}},
		{ID: "u6", Role: "user", Texts: []string{"[shell_command $ unclosed]"}},
		{ID: "u7", Role: "user", Texts: []string{"real prompt"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	var outputs []string
	for _, m := range []parser.Message{res.Messages[0], res.Messages[1], res.Messages[2], res.Messages[4]} {
		outputs = append(outputs, m.Parts[0].Payload.(parser.MarkerPayload).Output)
	}
	if want := []string{"## main...origin/main", "main -> main\nwarning: <x> &amp;", "", "failed"}; !reflect.DeepEqual(outputs, want) {
		t.Errorf("outputs = %q, want %q", outputs, want)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	j, _ := json.Marshal(res.Messages[2].Parts[0].Payload)
	if string(j) != `{"marker":"shell_command","text":"$ true"}` {
		t.Errorf("payload without output = %s", j)
	}
}

func TestParseCompaction(t *testing.T) {
	boundary := map[string]any{
		"type": "system", "subtype": "compact_boundary", "uuid": "cb", "parentUuid": nil, "logicalParentUuid": "a1",
		"timestamp": "2026-09-01T10:01:00.000Z", "content": "Conversation compacted", "compactMetadata": map[string]any{"trigger": "auto"},
	}
	summary := userLine("s1", "cb", "2026-09-01T10:01:00.100Z", "This session is being continued…")
	summary["isCompactSummary"] = true
	summary["isVisibleInTranscriptOnly"] = true
	// A summary with no boundary before it is a marker too.
	lone := userLine("s2", "a2", "2026-09-01T10:03:00.000Z", []any{text("Second summary")})
	lone["isCompactSummary"] = true
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "before"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("old answer"), nil),
		boundary,
		summary,
		userLine("u2", "s1", "2026-09-01T10:02:00.000Z", "after"),
		asstLine("a2", "u2", "2026-09-01T10:02:01.000Z", "msg_2", text("new answer"), nil),
		lone,
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"before"}},
		{ID: "a1", Role: "assistant", Texts: []string{"old answer"}},
		{ID: "cb", Role: "assistant", Texts: []string{"[compaction Conversation compacted]"}},
		{ID: "s1", Role: "user", Texts: []string{"[compaction This session is being continued…]"}},
		{ID: "u2", Role: "user", Texts: []string{"after"}},
		{ID: "a2", Role: "assistant", Texts: []string{"new answer"}},
		{ID: "s2", Role: "user", Texts: []string{"[compaction Second summary]"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}

	// A boundary without a summary keeps its own text.
	res = parse(t, jsonl(t, userLine("u1", "", "2026-09-01T10:00:00.000Z", "before"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("old answer"), nil), boundary), nil)
	if got := flatten(res.Messages); len(got) != 3 || got[2].Texts[0] != "[compaction Conversation compacted]" {
		t.Errorf("got %+v", got)
	}
}

func TestParseLineTypes(t *testing.T) {
	// Every Raw-only line type and system subtype of claude-code.md §3.1, on
	// the path, gives neither a Part nor a warning, and doesn't break an
	// assistant group.
	types := []string{
		"summary", "custom-title", "ai-title", "file-history-snapshot", "file-history-delta", "last-prompt",
		"mode", "permission-mode", "queue-operation", "progress", "atis-latch", "bridge-session", "cost-state",
		"agent-name", "pr-link", "frame-link", "artifact-autoreact-ledger", "artifact-comment-monitor", "continued-in",
		"attachment",
	}
	subtypes := []string{"turn_duration", "local_command", "away_summary", "stop_hook_summary", "informational", "api_error", "bridge_status"}
	lines := []map[string]any{
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("one"), nil),
	}
	parent := "a1"
	add := func(l map[string]any) {
		id := fmt.Sprintf("x%d", len(lines))
		l["uuid"], l["parentUuid"], l["timestamp"] = id, parent, "2026-09-01T10:00:02.000Z"
		lines = append(lines, l)
		parent = id
	}
	for _, typ := range types {
		add(map[string]any{"type": typ, "attachment": map[string]any{"type": "reminder"}})
	}
	for _, st := range subtypes {
		add(map[string]any{"type": "system", "subtype": st, "content": "x"})
	}
	lines = append(lines, asstLine("a2", parent, "2026-09-01T10:00:03.000Z", "msg_1", text("two"), nil))
	res := parse(t, jsonl(t, lines...), nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"one", "two"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseUnknownLineTypes(t *testing.T) {
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", text("one"), nil),
		// On the path: an unknown Part in its own Message, plus a warning.
		map[string]any{"type": "brand_new", "uuid": "n1", "parentUuid": "a1", "timestamp": "2026-09-01T10:00:02.000Z", "data": 1},
		map[string]any{"type": "system", "subtype": "shiny", "uuid": "n2", "parentUuid": "n1", "timestamp": "2026-09-01T10:00:02.500Z"},
		// Off the path: no uuid, and an abandoned branch. A warning only.
		map[string]any{"type": "brand_new", "data": 2},
		map[string]any{"type": "other_new", "uuid": "off", "parentUuid": "u1"},
		asstLine("a2", "n2", "2026-09-01T10:00:03.000Z", "msg_1", text("two"), nil),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"one"}},
		{ID: "n1", Role: "assistant", Texts: []string{"[unknown brand_new]"}},
		{ID: "n2", Role: "assistant", Texts: []string{"[unknown system:shiny]"}},
		{ID: "a2", Role: "assistant", Texts: []string{"two"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	u := res.Messages[2].Parts[0].Payload.(parser.UnknownPayload)
	if !strings.Contains(u.Excerpt, `"data":1`) {
		t.Errorf("excerpt = %q", u.Excerpt)
	}
	got := map[string]int{}
	for _, w := range res.Warnings {
		got[w.Kind+"/"+w.SourceType] = w.Count
	}
	wantW := map[string]int{"unknown_type/brand_new": 2, "unknown_type/system:shiny": 1, "unknown_type/other_new": 1}
	if !reflect.DeepEqual(got, wantW) {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseUserContent(t *testing.T) {
	// The user-content table of claude-code.md §3.3.
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	meta := userLine("m1", "u1", "2026-09-01T10:00:01.000Z", "injected")
	meta["isMeta"] = true
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "plain string"),
		meta,
		userLine("u2", "m1", "2026-09-01T10:00:02.000Z", []any{
			text("blocks"),
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
			map[string]any{"type": "document", "title": "spec.pdf", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": ""}},
			map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "text/plain", "data": ""}},
			map[string]any{"type": "tool_result", "tool_use_id": "nope", "content": "x"},
			map[string]any{"type": "mystery", "v": 1},
		}),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"plain string"}},
		{ID: "u2", Role: "user", Texts: []string{"blocks", "[image image/png]", "[attachment spec.pdf]", "[attachment text/plain]", "[unknown mystery]"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	got := map[string]int{}
	for _, w := range res.Warnings {
		got[w.Kind+"/"+w.SourceType] = w.Count
	}
	if !reflect.DeepEqual(got, map[string]int{"unknown_type/mystery": 1, "orphan/tool_result": 1}) {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseAssistantBlocks(t *testing.T) {
	// The assistant block table of claude-code.md §3.3.
	apiErr := asstLine("e1", "a5", "2026-09-01T10:00:06.000Z", "msg_err", text("API Error: overloaded"), nil)
	apiErr["isApiErrorMessage"] = true
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", map[string]any{"type": "thinking", "thinking": "ponder", "signature": "sig"}, nil),
		asstLine("a2", "a1", "2026-09-01T10:00:02.000Z", "msg_1", map[string]any{"type": "thinking", "thinking": "", "signature": "sig"}, nil),
		asstLine("a3", "a2", "2026-09-01T10:00:03.000Z", "msg_1", map[string]any{"type": "redacted_thinking", "data": "xx"}, nil),
		asstLine("a4", "a3", "2026-09-01T10:00:04.000Z", "msg_1", map[string]any{"type": "server_tool_use", "id": "s1"}, nil),
		asstLine("a5", "a4", "2026-09-01T10:00:05.000Z", "msg_1", text("answer"), nil),
		apiErr,
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"[thinking ponder]", "[unknown server_tool_use]", "answer"}},
		{ID: "e1", Role: "assistant", Texts: []string{"API Error: overloaded"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != "unknown_type" || res.Warnings[0].SourceType != "server_tool_use" ||
		!strings.Contains(res.Warnings[0].FirstExcerpt, `"id":"s1"`) {
		t.Errorf("warnings = %+v", res.Warnings)
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
	line["parentUuid"] = nil
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
	if res.Session.SpawningCallID != "toolu_9" || len(res.Warnings) != 0 {
		t.Errorf("spawning call = %q, warnings = %+v", res.Session.SpawningCallID, res.Warnings)
	}

	// Without its .meta.json the child has no title or spawning call.
	res, _ = New().Parse(parser.Input{NativeID: sess + "/agent-a1b2", Main: jsonl(t, line)})
	if res.Session.SpawningCallID != "" || res.Session.Title != "" || len(res.Warnings) != 1 ||
		res.Warnings[0] != (parser.Warning{Kind: "missing_field", SourceType: "meta.json", Count: 1}) {
		t.Errorf("session = %+v, warnings = %+v", res.Session, res.Warnings)
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
		{ID: "a1", Role: "assistant", Texts: []string{"[tool Bash ok]", "one"}},
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

// parseFixture parses testdata/<name>.jsonl with every file under
// testdata/tool-results as a tool-results attachment.
func parseFixture(t *testing.T, name string) parser.Result {
	t.Helper()
	main, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	att := map[string][]byte{}
	ents, _ := os.ReadDir(filepath.Join("testdata", "tool-results"))
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join("testdata", "tool-results", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		att["-Users-ted-src-app/"+sess+"/tool-results/"+e.Name()] = b
	}
	return parse(t, main, att)
}

func TestParseToolCallsAndImages(t *testing.T) {
	res := parseFixture(t, "tools")

	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"look at this", "[image image/png]"}},
		{ID: "a1", Role: "assistant", Texts: []string{
			"[tool Bash ok]", "[tool Bash error]", "[tool Edit ok]", "[tool Bash ok]", "[tool Grep ok]",
			"[tool Read ok]", "[tool ToolSearch ok]", "[tool Screenshot ok]", "[image image/png]",
			"Here is a chart:", "[image image/png]", "[tool Bash pending]",
		}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages:\n got %+v\nwant %+v", got, want)
	}

	calls := map[string]parser.ToolCallPayload{}
	for _, p := range res.Messages[1].Parts {
		if c, ok := p.Payload.(parser.ToolCallPayload); ok {
			calls[c.CallID] = c
		}
	}
	out := func(id string) string {
		t.Helper()
		if calls[id].Output == nil {
			t.Fatalf("%s: output is nil", id)
		}
		return *calls[id].Output
	}

	var in struct{ Command string }
	if c := calls["t_ok"]; c.Name != "Bash" || json.Unmarshal(c.Input, &in) != nil || in.Command != "echo <b>hi</b>" {
		t.Errorf("t_ok = %+v", c)
	}
	if got := out("t_ok"); got != "<b>hi</b>" {
		t.Errorf("t_ok output = %q", got)
	}
	if got := out("t_err"); got != "exit 1" {
		t.Errorf("t_err output = %q", got)
	}
	if d := calls["t_edit"].Diff; d == nil || *d != (parser.Diff{Path: "/Users/ted/src/app/main.go", Old: "a\nb", New: "a\nc"}) {
		t.Errorf("t_edit diff = %+v", d)
	}
	if calls["t_ok"].Diff != nil {
		t.Error("a non-Edit call has a diff")
	}
	if got := out("t_spill"); got != "FULL OUTPUT of bdqdsmwk4 � end\n" {
		t.Errorf("spilled output not stitched from the named file: %q", got)
	}
	if got := out("t_fallback"); got != "FULL OUTPUT of t_fallback\n" {
		t.Errorf("spilled output not stitched from <tool_use_id>.txt: %q", got)
	}
	if got := out("t_missing"); !strings.HasPrefix(got, "<persisted-output>") || !strings.Contains(got, "head of output") {
		t.Errorf("missing spill should keep the marker: %q", got)
	}
	if got := out("t_blocks"); got != "found\n[tool reference: Read]\ndone" {
		t.Errorf("block-list output = %q", got)
	}
	if got := out("t_shot"); got != "captured" {
		t.Errorf("t_shot output = %q", got)
	}
	if c := calls["t_pending"]; c.Output != nil || c.Status != "pending" {
		t.Errorf("pending call = %+v", c)
	}
	for id, c := range calls {
		if c.ChildSessions == nil || len(c.ChildSessions) != 0 {
			t.Errorf("%s: child_sessions = %v, want []", id, c.ChildSessions)
		}
	}

	// Three image Parts, one image: content-addressed bytes.
	if len(res.Images) != 3 {
		t.Fatalf("images = %d", len(res.Images))
	}
	img := res.Images[0]
	if img.MIME != "image/png" || len(img.Bytes) == 0 || img.SHA256 != res.Images[2].SHA256 {
		t.Errorf("image = %+v", img)
	}
	if p := res.Messages[0].Parts[1].Payload.(parser.ImagePayload); p.SHA256 != img.SHA256 {
		t.Errorf("image Part sha = %q, want %q", p.SHA256, img.SHA256)
	}

	var orphans int
	for _, w := range res.Warnings {
		if w.Kind == "orphan" && w.SourceType == "tool_result" {
			orphans = w.Count
		}
	}
	if orphans != 1 {
		t.Errorf("warnings = %+v, want one tool_result orphan", res.Warnings)
	}
}

func TestParseResultBeforeCallDoesNotMerge(t *testing.T) {
	// A result must come after its call on the path.
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", []any{text("go"), map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "early"}}),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{}}, nil),
	)
	res := parse(t, main, nil)
	c := res.Messages[1].Parts[0].Payload.(parser.ToolCallPayload)
	if c.Status != "pending" {
		t.Errorf("status = %q", c.Status)
	}
}

func TestParseImageWithoutBytesWarns(t *testing.T) {
	img := map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}}
	res := parse(t, jsonl(t, userLine("u1", "", "2026-09-01T10:00:00.000Z", []any{text("see"), img})), nil)
	if got := flatten(res.Messages); len(got) != 1 || len(got[0].Texts) != 1 {
		t.Errorf("messages = %+v", got)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != "unknown_type" || res.Warnings[0].SourceType != "image" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseAgentCallsLinkChildSessions(t *testing.T) {
	call := func(uuid, parent, id, name string) map[string]any {
		return asstLine(uuid, parent, "2026-09-01T10:00:01.000Z", "msg_"+uuid, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}, nil)
	}
	result := func(uuid, parent, id string, tur any) map[string]any {
		l := userLine(uuid, parent, "2026-09-01T10:00:02.000Z", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "done"}})
		if tur != nil {
			l["toolUseResult"] = tur
		}
		return l
	}
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		call("a1", "u1", "t_agent", "Agent"),
		result("r1", "a1", "t_agent", map[string]any{"agentId": "abc", "status": "completed"}),
		call("a2", "r1", "t_task", "Task"),
		result("r2", "a2", "t_task", map[string]any{"agentId": "def"}),
		call("a3", "r2", "t_bash", "Bash"),
		result("r3", "a3", "t_bash", map[string]any{"agentId": "nope"}),
		call("a4", "r3", "t_str", "Agent"),
		result("r4", "a4", "t_str", "Error: interrupted"),
		// Two results on one line share its toolUseResult, so neither can
		// claim its agentId.
		call("a5", "r4", "t_p1", "Agent"),
		call("a6", "a5", "t_p2", "Agent"),
		map[string]any{"type": "user", "uuid": "r5", "parentUuid": "a6", "timestamp": "2026-09-01T10:00:04.000Z",
			"toolUseResult": map[string]any{"agentId": "ghi"},
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t_p1", "content": "one"},
				map[string]any{"type": "tool_result", "tool_use_id": "t_p2", "content": "two"},
			}}},
	)
	children := func(res parser.Result) map[string][]string {
		out := map[string][]string{}
		for _, m := range res.Messages {
			for _, p := range m.Parts {
				if c, ok := p.Payload.(parser.ToolCallPayload); ok {
					out[c.CallID] = c.ChildSessions
				}
			}
		}
		return out
	}
	want := map[string][]string{"t_agent": {sess + "/agent-abc"}, "t_task": {sess + "/agent-def"}, "t_bash": {}, "t_str": {}, "t_p1": {}, "t_p2": {}}
	if got := children(parse(t, main, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("child_sessions = %v", got)
	}

	// A nested Child Session's call links to a sibling filed under the top-level Session.
	res, err := New().Parse(parser.Input{NativeID: sess + "/agent-abc", Main: main})
	if err != nil {
		t.Fatal(err)
	}
	if got := children(res)["t_agent"]; !reflect.DeepEqual(got, []string{sess + "/agent-abc"}) {
		t.Errorf("nested child_sessions = %v", got)
	}
}

func TestParseParallelCallResultsOffThePath(t *testing.T) {
	call := func(uuid, parent, id string) map[string]any {
		return asstLine(uuid, parent, "2026-09-01T10:00:01.000Z", "msg_1", map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{}}, nil)
	}
	result := func(uuid, parent, id, out string) map[string]any {
		return userLine(uuid, parent, "2026-09-01T10:00:02.000Z", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}})
	}
	// Claude Code chains the second parallel call onto the first, so the
	// first call's result hangs off it as a side branch.
	res := parse(t, jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		call("a1", "u1", "t1"),
		call("a2", "a1", "t2"),
		result("r1", "a1", "t1", "one"),
		result("r2", "a2", "t2", "two"),
		// A side result for a call already answered on the path loses.
		result("r2x", "a2", "t2", "stale"),
		asstLine("a3", "r2", "2026-09-01T10:00:03.000Z", "msg_2", text("done"), nil),
	), nil)
	if len(res.Messages) != 3 || len(res.Messages[1].Parts) != 2 {
		t.Fatalf("messages = %+v", res.Messages)
	}
	for i, want := range []string{"one", "two"} {
		c := res.Messages[1].Parts[i].Payload.(parser.ToolCallPayload)
		if c.Status != parser.StatusOK || c.Output == nil || *c.Output != want {
			t.Errorf("call %s: status %q, output %v", c.CallID, c.Status, c.Output)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// notification is a <task-notification> block with the given inner lines.
func notification(inner ...string) string {
	return "<task-notification>\n" + strings.Join(inner, "\n") + "\n</task-notification>"
}

func TestParseTaskNotifications(t *testing.T) {
	agentDone := notification(
		"<task-id>ab5a75fccc3622763</task-id>",
		"<tool-use-id>t_agent</tool-use-id>",
		"<output-file>/tmp/tasks/ab5a75fccc3622763.output</output-file>",
		"<status>completed</status>",
		`<summary>Agent "Standards review" finished</summary>`,
		"<note>It may notify more than once.</note>",
		"<result>## Findings\nNone.\n</result>",
		"<usage><subagent_tokens>52749</subagent_tokens><tool_uses>6</tool_uses><duration_ms>47425</duration_ms></usage>",
		"<worktree><worktreePath>/tmp/wt</worktreePath></worktree>",
	)
	cmdFailed := notification(
		"<task-id>b9r9tn0o3</task-id>",
		"<tool-use-id>t_gone</tool-use-id>",
		"<status>failed</status>",
		`<summary>Background command "Render PDF" failed with exit code 144</summary>`,
	)
	killed := notification(
		"<task-id>bttwowpoo</task-id>",
		"<tool-use-id>t_agent</tool-use-id>",
		"<status>killed</status>",
		`<summary>Background command "Wait" was stopped</summary>`,
	)
	event := notification(
		"<task-id>bwlkc6esw</task-id>",
		`<summary>Monitor event: "rebuild progress"</summary>`,
		"<event>\nstep 1 done\nstep 2 done\n</event>",
		"If this event is something the user would act on now, send a PushNotification.",
	)
	fromOrigin := userLine("n1", "r1", "2026-09-01T10:00:03.000Z", agentDone)
	fromOrigin["origin"] = map[string]any{"kind": "task-notification"}
	queued := func(uuid, parent, prompt string) map[string]any {
		return map[string]any{"type": "attachment", "uuid": uuid, "parentUuid": parent, "timestamp": "2026-09-01T10:00:04.000Z",
			"attachment": map[string]any{"type": "queued_command", "prompt": prompt, "commandMode": "task-notification"}}
	}
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-01T10:00:01.000Z", "msg_1", map[string]any{"type": "tool_use", "id": "t_agent", "name": "Agent", "input": map[string]any{}}, nil),
		userLine("r1", "a1", "2026-09-01T10:00:02.000Z", []any{map[string]any{"type": "tool_result", "tool_use_id": "t_agent", "content": "launched"}}),
		fromOrigin,
		queued("n2", "n1", cmdFailed),
		// No origin: the text alone marks it.
		userLine("n3", "n2", "2026-09-01T10:00:05.000Z", killed),
		queued("n4", "n3", event),
		userLine("u2", "n4", "2026-09-01T10:00:06.000Z", "thanks"),
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"[tool Agent ok]"}},
		{ID: "n1", Role: "user", Texts: []string{`[task_notification Agent "Standards review" finished]`}},
		{ID: "n2", Role: "user", Texts: []string{`[task_notification Background command "Render PDF" failed with exit code 144]`}},
		{ID: "n3", Role: "user", Texts: []string{`[task_notification Background command "Wait" was stopped]`}},
		{ID: "n4", Role: "user", Texts: []string{`[task_notification Monitor event: "rebuild progress"]`}},
		{ID: "u2", Role: "user", Texts: []string{"thanks"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages:\n got %+v\nwant %+v", got, want)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}

	marker := func(i int) parser.MarkerPayload { return res.Messages[i].Parts[0].Payload.(parser.MarkerPayload) }
	tasks := []parser.TaskPayload{
		{Status: "completed", ToolUseID: "t_agent", CallPart: "a1.0", Result: "## Findings\nNone.", Tokens: 52749, ToolUses: 6, DurationMS: 47425},
		{Status: "failed", ToolUseID: "t_gone"},
		{Status: "killed", ToolUseID: "t_agent", CallPart: "a1.0"},
		{},
	}
	for i, w := range tasks {
		if got := marker(i + 2).Task; got == nil || *got != w {
			t.Errorf("message %d task = %+v, want %+v", i+2, got, w)
		}
	}
	if got := marker(5).Output; got != "step 1 done\nstep 2 done" {
		t.Errorf("event output = %q", got)
	}
	if got := marker(2).Output; got != "" {
		t.Errorf("agent output = %q", got)
	}
	call := res.Messages[1].Parts[0].Payload.(parser.ToolCallPayload)
	if want := []string{"n1.0", "n3.0"}; !reflect.DeepEqual(call.Notifications, want) {
		t.Errorf("call notifications = %v, want %v", call.Notifications, want)
	}
	j, _ := json.Marshal(marker(3))
	if want := `{"marker":"task_notification","text":"Background command \"Render PDF\" failed with exit code 144","task":{"status":"failed","tool_use_id":"t_gone"}}`; string(j) != want {
		t.Errorf("payload = %s\nwant      %s", j, want)
	}
}

func TestParseMalformedTaskNotifications(t *testing.T) {
	unclosed := "<task-notification>\n<status>completed</status>\n<summary>Agent finished</summary>"
	noSummary := notification("<status>completed</status>", "<summary> </summary>")
	fromOrigin := userLine("n1", "u1", "2026-09-01T10:00:01.000Z", unclosed)
	fromOrigin["origin"] = map[string]any{"kind": "task-notification"}
	main := jsonl(t,
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "go"),
		fromOrigin,
		map[string]any{"type": "attachment", "uuid": "n2", "parentUuid": "n1", "timestamp": "2026-09-01T10:00:02.000Z",
			"attachment": map[string]any{"type": "queued_command", "prompt": noSummary, "commandMode": "task-notification"}},
	)
	res := parse(t, main, nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "n1", Role: "user", Texts: []string{unclosed}},
		{ID: "n2", Role: "user", Texts: []string{noSummary}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("messages:\n got %+v\nwant %+v", got, want)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != (parser.Warning{Kind: "missing_field", SourceType: "task-notification", Count: 2, FirstExcerpt: res.Warnings[0].FirstExcerpt}) {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

func TestParseTaskNotificationResultQuotingTags(t *testing.T) {
	// A sub-agent's report can quote the very tags the block is made of.
	quoted := "Found `</task-notification>` and <status>failed</status> and <usage>x</usage> in the diff."
	block := notification(
		"<tool-use-id>t_1</tool-use-id>",
		"<status>completed</status>",
		"<summary>Agent Review finished</summary>",
		"<result>"+quoted+"\n</result>",
		"<usage><subagent_tokens>10</subagent_tokens><tool_uses>2</tool_uses><duration_ms>3000</duration_ms></usage>",
	)
	res := parse(t, jsonl(t, userLine("n1", "", "2026-09-01T10:00:00.000Z", block)), nil)
	mp, ok := res.Messages[0].Parts[0].Payload.(parser.MarkerPayload)
	if !ok {
		t.Fatalf("not a marker: %+v", flatten(res.Messages))
	}
	want := parser.TaskPayload{Status: "completed", ToolUseID: "t_1", Result: quoted, Tokens: 10, ToolUses: 2, DurationMS: 3000}
	if mp.Task == nil || *mp.Task != want {
		t.Errorf("task = %+v\nwant   %+v", mp.Task, want)
	}
}

// TestScheduledTaskFire: a fire line is a scheduled_task marker with its
// prompt as the output, and the isMeta prompt line after it stays Raw only
// (claude-code.md §3.1).
func TestScheduledTaskFire(t *testing.T) {
	fire := func(id, parent, ts, prompt string) map[string]any {
		l := map[string]any{
			"type": "system", "subtype": "scheduled_task_fire", "uuid": id, "parentUuid": parent, "timestamp": ts,
			"content": "Claude resuming /loop wakeup (Sep 28 4:20pm)", "taskId": "b6438d94", "cron": "20 16 * * *",
			"taskKind": "loop", "cronKind": "loop",
		}
		if prompt != "" {
			l["prompt"] = prompt
		}
		return l
	}
	meta := userLine("m1", "f1", "2026-09-28T20:20:00.780Z", "Check the renders.")
	meta["isMeta"] = true
	lines := []map[string]any{
		userLine("u1", "", "2026-09-28T20:00:00.000Z", "go"),
		asstLine("a1", "u1", "2026-09-28T20:00:01.000Z", "msg_1", text("one"), nil),
		fire("f1", "a1", "2026-09-28T20:20:00.770Z", "Check the renders."),
		meta,
		asstLine("a2", "m1", "2026-09-28T20:20:01.000Z", "msg_2", text("two"), nil),
		fire("f2", "a2", "2026-09-28T20:41:00.000Z", ""),
	}
	res := parse(t, jsonl(t, lines...), nil)
	want := []flat{
		{ID: "u1", Role: "user", Texts: []string{"go"}},
		{ID: "a1", Role: "assistant", Texts: []string{"one"}},
		{ID: "f1", Role: "user", Texts: []string{"[scheduled_task Claude resuming /loop wakeup (Sep 28 4:20pm)]"}},
		{ID: "a2", Role: "assistant", Texts: []string{"two"}},
		{ID: "f2", Role: "user", Texts: []string{"[scheduled_task Claude resuming /loop wakeup (Sep 28 4:20pm)]"}},
	}
	if got := flatten(res.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if out := res.Messages[2].Parts[0].Payload.(parser.MarkerPayload).Output; out != "Check the renders." {
		t.Errorf("output = %q", out)
	}
	if out := res.Messages[4].Parts[0].Payload.(parser.MarkerPayload).Output; out != "" {
		t.Errorf("output without prompt = %q", out)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	if got := parser.TitleCandidate(res.Messages); got != "go" {
		t.Errorf("title candidate = %q", got)
	}
}
