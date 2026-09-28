package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

const (
	thread  = "01a0d387-4530-74a2-8c7a-d5f10cc15942"
	rollout = "01a0d399-aaaa-7bbb-8ccc-9ddddeeeeeee"
	parent  = "01a0a0c3-6416-72d2-ab61-cb1873b7787e"
	mainKey = "rollout-2026-09-24T09-07-32-" + thread + ".jsonl"
	contKey = "rollout-2026-09-24T10-00-00-" + thread + "_" + rollout + ".jsonl"
)

func TestMapKey(t *testing.T) {
	m := func(role string) parser.Mapping {
		return parser.Mapping{NativeID: thread, Role: role, Layout: "jsonl", LayoutRank: 1}
	}
	for _, c := range []struct {
		key  string
		want parser.Mapping
		ok   bool
	}{
		{mainKey, m("main"), true},
		{contKey, m("attachment"), true},
		{mainKey + ".zst", parser.Mapping{}, false},
		{"sessions/2026/09/24/" + mainKey, parser.Mapping{}, false},
		{"rollout-2026-09-24T09-07-32-not-a-uuid.jsonl", parser.Mapping{}, false},
		{"rollout-2026-09-24-" + thread + ".jsonl", parser.Mapping{}, false},
		{"rollout-2026-09-24T10-00-00-" + thread + "_x.jsonl", parser.Mapping{}, false},
		{"history.jsonl", parser.Mapping{}, false},
	} {
		got, ok := New().MapKey(c.key)
		if ok != c.ok || got != c.want {
			t.Errorf("MapKey(%q) = %+v, %v; want %+v, %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

// fixture parses a redacted real rollout from testdata.
func fixture(t *testing.T, name string) (parser.Result, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := New().MapKey(name)
	if !ok {
		t.Fatalf("MapKey(%q) not ok", name)
	}
	res, err := New().Parse(parser.Input{NativeID: m.NativeID, MainKey: name, Main: b})
	if err != nil {
		t.Fatal(err)
	}
	return res, b
}

// kinds lists a Message's Part kinds.
func kinds(m parser.Message) []string {
	var ks []string
	for _, p := range m.Parts {
		ks = append(ks, p.Kind)
	}
	return ks
}

func texts(m parser.Message) []string {
	var ts []string
	for _, p := range m.Parts {
		switch pl := p.Payload.(type) {
		case parser.TextPayload:
			ts = append(ts, pl.Text)
		case parser.ThinkingPayload:
			ts = append(ts, pl.Text)
		}
	}
	return ts
}

// responseUsage sums each distinct response's usage in a rollout, the
// independent check on per-Message usage.
func responseUsage(t *testing.T, b []byte) parser.Usage {
	t.Helper()
	var total parser.Usage
	seen := map[string]bool{}
	for _, raw := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				ResponseID string           `json:"response_id"`
				U          map[string]int64 `json:"usage"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		if l.Type != "token_usage_record" || seen[l.Payload.ResponseID] {
			continue
		}
		seen[l.Payload.ResponseID] = true
		u := l.Payload.U
		total.Add(parser.Usage{Input: u["input_tokens"], Output: u["output_tokens"], CacheRead: u["cached_input_tokens"],
			CacheWrite: u["cache_write_input_tokens"], Reasoning: u["reasoning_output_tokens"]})
	}
	return total
}

// The real rollouts parse with no warnings: injected context and developer
// messages are Raw only, each turn's commentary, reasoning and tool calls
// form one assistant Message, and each call has its output.
func TestRealRollouts(t *testing.T) {
	for _, c := range []struct {
		name, version, model string
		users, calls         int
		thinking             int
	}{
		{"rollout-2026-09-14T12-32-34-01a0a0c3-6416-72d2-ab61-cb1873b7787e.jsonl", "0.154.0-alpha.6.2", "gpt-6-astra", 20, 27, 2},
		{"rollout-2026-09-14T14-41-51-01a0a139-c023-7610-8e4a-f7104bf801f1.jsonl", "0.154.0-alpha.6.2", "gpt-6-astra", 10, 6, 0},
		{"rollout-2026-09-24T09-07-32-01a0d387-4530-74a2-8c7a-d5f10cc15942.jsonl", "0.156.1", "gpt-6-sol", 1, 25, 0},
	} {
		t.Run(c.version+"/"+c.name[28:36], func(t *testing.T) {
			res, raw := fixture(t, c.name)
			if len(res.Warnings) != 0 {
				t.Errorf("warnings %+v", res.Warnings)
			}
			s := res.Session
			if s.Cwd != "/Users/user/src/app" || s.SourceVersion != c.version || s.Title != "" ||
				s.StartedAt == 0 || s.LastActivityAt <= s.StartedAt || s.ParentNativeID != "" || s.ForkedFromNativeID != "" {
				t.Errorf("session %+v", s)
			}
			var users, calls, thinking int
			var usage parser.Usage
			for i, m := range res.Messages {
				if m.ID != strconv.Itoa(mustAtoi(t, m.ID)) {
					t.Errorf("message id %q is not an ordinal", m.ID)
				}
				for _, tx := range texts(m) {
					if strings.Contains(tx, "<") || strings.Contains(tx, "redacted developer") {
						t.Errorf("message %s shows injected context %q", m.ID, tx)
					}
				}
				switch m.Role {
				case parser.MessageUser:
					users++
					if len(m.Parts) == 0 || !strings.HasPrefix(texts(m)[0], "redacted user ") {
						t.Errorf("user message %s = %+v", m.ID, m.Parts)
					}
				case parser.MessageAssistant:
					if i == 0 || res.Messages[i-1].Role != parser.MessageUser {
						t.Errorf("assistant message %s doesn't follow a prompt: one turn should be one Message", m.ID)
					}
					if m.Model != c.model || m.Provider != "openai" || m.Usage == nil {
						t.Errorf("assistant message %s: model %q provider %q usage %v", m.ID, m.Model, m.Provider, m.Usage)
					} else {
						usage.Add(*m.Usage)
					}
				}
				for j, p := range m.Parts {
					if want := m.ID + "." + strconv.Itoa(j); p.ID != want {
						t.Errorf("part id %q, want %q", p.ID, want)
					}
					switch pl := p.Payload.(type) {
					case parser.ToolCallPayload:
						calls++
						if pl.Status != parser.StatusOK || pl.Output == nil || !strings.HasPrefix(*pl.Output, "redacted output") ||
							pl.Name == "" || len(pl.Input) == 0 || pl.Diff != nil || pl.ChildSessions == nil {
							t.Errorf("tool call %s = %+v", p.ID, pl)
						}
					case parser.ThinkingPayload:
						thinking++
					}
				}
			}
			if users != c.users || calls != c.calls || thinking != c.thinking {
				t.Errorf("users %d calls %d thinking %d; want %d %d %d", users, calls, thinking, c.users, c.calls, c.thinking)
			}
			if want := responseUsage(t, raw); usage != want {
				t.Errorf("usage over Messages %+v, want each response once: %+v", usage, want)
			}
		})
	}
}

func mustAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Errorf("id %q: %v", s, err)
	}
	return n
}

// One real turn in full: commentary, two exec calls and the final answer
// in one assistant Message, output blocks joined by newlines, and usage
// summed over the turn's responses.
func TestRealTurn(t *testing.T) {
	res, _ := fixture(t, "rollout-2026-09-14T12-32-34-01a0a0c3-6416-72d2-ab61-cb1873b7787e.jsonl")
	u, a := res.Messages[0], res.Messages[1]
	if u.ID != "8" || !reflect.DeepEqual(texts(u), []string{"redacted user 8.0"}) {
		t.Errorf("first prompt %+v", u)
	}
	if a.ID != "11" || !slices.Equal(kinds(a), []string{"text", "tool_call", "tool_call", "text"}) {
		t.Fatalf("first turn %s: %v", a.ID, kinds(a))
	}
	tc := a.Parts[1].Payload.(parser.ToolCallPayload)
	if tc.CallID != "call_euUGnAW4qqfcRuPFaaOGXNO7" || tc.Name != "exec" || string(tc.Input) != `"redacted input 12"` ||
		*tc.Output != "redacted output 15.0\nredacted output 15.1" {
		t.Errorf("custom tool call %+v output %q", tc, *tc.Output)
	}
	// Four responses in the turn.
	want := parser.Usage{Input: 109503, Output: 405, CacheRead: 87424, Reasoning: 59}
	if *a.Usage != want {
		t.Errorf("turn usage %+v, want %+v", *a.Usage, want)
	}
	var sums []string
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if th, ok := p.Payload.(parser.ThinkingPayload); ok {
				sums = append(sums, th.Text)
			}
		}
	}
	if !slices.Equal(sums, []string{"redacted summary 324.0", "redacted summary 331.0"}) {
		t.Errorf("reasoning summaries %q", sums)
	}

	cli, _ := fixture(t, "rollout-2026-09-24T09-07-32-01a0d387-4530-74a2-8c7a-d5f10cc15942.jsonl")
	for _, p := range cli.Messages[1].Parts {
		if tc, ok := p.Payload.(parser.ToolCallPayload); ok && tc.CallID == "call_6PbKneg5HhZh3X4Yax2Ybxw4" {
			if tc.Name != "wait" || string(tc.Input) != `{"cell_id":"14","yield_time_ms":1000}` || *tc.Output != "redacted output 148.0\nredacted output 148.1" {
				t.Errorf("function call %+v output %q", tc, *tc.Output)
			}
			return
		}
	}
	t.Error("function call not found")
}

// rollout builds rollout lines from (type, payload) pairs, numbering
// ordinals from first, a second apart.
type rl struct {
	typ     string
	payload map[string]any
}

func build(t *testing.T, first int, lines ...rl) []byte {
	t.Helper()
	var b strings.Builder
	for i, l := range lines {
		j, err := json.Marshal(map[string]any{
			"timestamp": "2026-09-24T13:" + pad(8+(first+i)/60) + ":" + pad((first+i)%60) + ".000Z",
			"ordinal":   first + i, "type": l.typ, "payload": l.payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func pad(n int) string { return strings.Repeat("0", 2-len(strconv.Itoa(n))) + strconv.Itoa(n) }

func meta(id string, extra map[string]any) rl {
	p := map[string]any{"session_id": id, "id": id, "timestamp": "2026-09-24T13:07:32.785Z", "cwd": "/Users/user/src/app",
		"originator": "codex-tui", "cli_version": "0.156.1", "source": "cli", "model_provider": "openai",
		"history_mode": "paginated", "base_instructions": map[string]any{"text": "redacted"}, "git": map[string]any{"branch": "main"}}
	for k, v := range extra {
		p[k] = v
	}
	return rl{"session_meta", p}
}

func turn(model, effort string) rl {
	return rl{"turn_context", map[string]any{"cwd": "/Users/user/src/app", "model": model, "effort": effort, "summary": "auto",
		"approval_policy": "on-request", "sandbox_policy": map[string]any{"type": "workspace-write"}}}
}

func user(blocks ...map[string]any) rl {
	return rl{"response_item", map[string]any{"type": "message", "role": "user", "content": blocks}}
}

func inText(s string) map[string]any { return map[string]any{"type": "input_text", "text": s} }

func say(phase, s string) rl {
	return rl{"response_item", map[string]any{"type": "message", "role": "assistant", "phase": phase,
		"content": []any{map[string]any{"type": "output_text", "text": s}}}}
}

func usage(resp string, in, out int) rl {
	u := map[string]any{"input_tokens": in, "cached_input_tokens": 1, "cache_write_input_tokens": 2, "output_tokens": out,
		"reasoning_output_tokens": 3, "total_tokens": in + out}
	return rl{"token_usage_record", map[string]any{"thread_id": thread, "turn_id": "t", "response_id": resp,
		"usage": u, "turn_token_usage": u, "thread_token_usage": u}}
}

func parse(t *testing.T, in parser.Input) parser.Result {
	t.Helper()
	if in.NativeID == "" {
		in.NativeID = thread
	}
	if in.MainKey == "" {
		in.MainKey = mainKey
	}
	res, err := New().Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func warnings(res parser.Result) []string {
	var ws []string
	for _, w := range res.Warnings {
		ws = append(ws, w.Kind+":"+w.SourceType+"×"+strconv.Itoa(w.Count))
	}
	return ws
}

// Each §3 mapping row not in the real rollouts: markers, compaction,
// images, tool call statuses, orphans and unknown types.
func TestMappingRows(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgo="
	main := build(t, 0,
		meta(thread, nil),                   // 0
		rl{"world_state", map[string]any{}}, // 1
		turn("gpt-6-sol", "medium"),         // 2: the first turn_context: no marker
		rl{"response_item", map[string]any{"type": "message", "role": "developer", "content": []any{inText("be nice")}}},                               // 3
		user(inText("<environment_context>\n  <cwd>/x</cwd>\n</environment_context>"), inText("<skill name=\"x\">\nbody\n</skill>")),                   // 4: injected only: no Message
		user(inText("Look at this"), map[string]any{"type": "input_image", "image_url": png}, map[string]any{"type": "input_audio", "audio_url": "x"}), // 5
		rl{"response_item", map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "First"}, map[string]any{"type": "summary_text", "text": "Second"}}, "encrypted_content": "x"}}, // 6
		rl{"response_item", map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "x"}},                                                      // 7: no Part
		rl{"response_item", map[string]any{"type": "function_call", "name": "shell", "arguments": `{"cmd":["ls"]}`, "call_id": "c1"}},                               // 8
		rl{"response_item", map[string]any{"type": "function_call", "name": "view", "arguments": `not json`, "call_id": "c2"}},                                      // 9
		rl{"response_item", map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": "*** Begin Patch", "call_id": "c3", "status": "failed"}},     // 10
		rl{"response_item", map[string]any{"type": "custom_tool_call", "name": "exec", "input": "x", "call_id": "c4", "status": "completed"}},                       // 11: pending
		rl{"response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "a.txt\n"}},                                                   // 12
		rl{"response_item", map[string]any{"type": "function_call_output", "call_id": "c2", "output": map[string]any{"content": "no such file", "success": false}}}, // 13
		rl{"response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "c3", "output": []any{
			inText("patch failed"), map[string]any{"type": "input_image", "image_url": png}, inText("see above")}}}, // 14
		rl{"response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "nope", "output": "lost"}}, // 15: orphan
		rl{"response_item", map[string]any{"type": "web_search_call", "status": "completed"}},                       // 16: unknown
		say("final_answer", "Done."),                           // 17
		usage("resp_1", 100, 10),                               // 18
		usage("resp_1", 100, 10),                               // 19: the same response again
		rl{"event_msg", map[string]any{"type": "token_count"}}, // 20
		usage("resp_2", 50, 5),                                 // 21
		turn("gpt-6-astra", "high"),                            // 22: model_change + thinking_level
		user(inText("Next")),                                   // 23
		say("final_answer", "Ok."),                             // 24
		rl{"compacted", map[string]any{"message": "Summary of the work so far"}}, // 25
		rl{"compacted", map[string]any{"message": ""}},                           // 26
		turn("gpt-6-astra", "low"),                                               // 27: thinking_level only
		rl{"inter_agent_communication", map[string]any{"x": 1}},                  // 28: unknown line type
	)
	main = append(main, []byte("{not json\n")...)
	res := parse(t, parser.Input{Main: main})

	type want struct {
		id, role string
		kinds    []string
	}
	var got []want
	for _, m := range res.Messages {
		got = append(got, want{m.ID, m.Role, kinds(m)})
	}
	wantMsgs := []want{
		{"5", "user", []string{"text", "image", "unknown"}},
		{"6", "assistant", []string{"thinking", "tool_call", "tool_call", "tool_call", "image", "tool_call", "unknown", "text"}},
		{"22", "assistant", []string{"marker", "marker"}},
		{"23", "user", []string{"text"}},
		{"24", "assistant", []string{"text"}},
		{"25", "assistant", []string{"marker"}},
		{"26", "assistant", []string{"marker"}},
		{"27", "assistant", []string{"marker"}},
		{"28", "assistant", []string{"unknown"}},
	}
	if !reflect.DeepEqual(got, wantMsgs) {
		t.Fatalf("messages:\n got  %v\n want %v", got, wantMsgs)
	}

	a := res.Messages[1]
	if a.Model != "gpt-6-sol" || a.Provider != "openai" || a.Timestamp != res.Messages[0].Timestamp+1000 {
		t.Errorf("assistant model %q provider %q ts %d", a.Model, a.Provider, a.Timestamp)
	}
	if want := (parser.Usage{Input: 150, Output: 15, CacheRead: 2, CacheWrite: 4, Reasoning: 6}); a.Usage == nil || *a.Usage != want {
		t.Errorf("usage %+v, want %+v", a.Usage, want)
	}
	if th := a.Parts[0].Payload.(parser.ThinkingPayload); th.Text != "First\n\nSecond" {
		t.Errorf("thinking %q", th.Text)
	}
	call := func(i int) parser.ToolCallPayload { return a.Parts[i].Payload.(parser.ToolCallPayload) }
	out := func(tc parser.ToolCallPayload) string {
		if tc.Output == nil {
			return "<nil>"
		}
		return *tc.Output
	}
	for i, w := range []struct{ input, status, output string }{
		{`{"cmd":["ls"]}`, "ok", "a.txt\n"},
		{`"not json"`, "error", "no such file"},
		{`"*** Begin Patch"`, "error", "patch failed\nsee above"},
		{`"x"`, "pending", "<nil>"},
	} {
		idx := []int{1, 2, 3, 5}[i]
		tc := call(idx)
		if string(tc.Input) != w.input || tc.Status != w.status || out(tc) != w.output || tc.Diff != nil || len(tc.ChildSessions) != 0 {
			t.Errorf("call %d = input %s status %s output %q", idx, tc.Input, tc.Status, out(tc))
		}
	}
	if img := a.Parts[4].Payload.(parser.ImagePayload); img.MIME != "image/png" || len(res.Images) != 2 || res.Images[0].SHA256 != img.SHA256 {
		t.Errorf("output image %+v, images %d", img, len(res.Images))
	}

	markers := func(m parser.Message) []parser.MarkerPayload {
		var ms []parser.MarkerPayload
		for _, p := range m.Parts {
			ms = append(ms, p.Payload.(parser.MarkerPayload))
		}
		return ms
	}
	for i, w := range [][]parser.MarkerPayload{
		{{Marker: "model_change", Text: "gpt-6-astra"}, {Marker: "thinking_level", Text: "high"}},
		{{Marker: "compaction", Text: "Summary of the work so far"}},
		{{Marker: "compaction", Text: parser.CompactionText}},
		{{Marker: "thinking_level", Text: "low"}},
	} {
		m := res.Messages[[]int{2, 5, 6, 7}[i]]
		if got := markers(m); !reflect.DeepEqual(got, w) {
			t.Errorf("message %s markers %+v, want %+v", m.ID, got, w)
		}
	}
	if m := res.Messages[4]; m.Model != "gpt-6-astra" {
		t.Errorf("model after the change %q", m.Model)
	}

	wantWarn := []string{"unknown_type:input_audio×1", "unknown_type:web_search_call×1", "unknown_type:inter_agent_communication×1",
		"bad_line:×1", "orphan:custom_tool_call_output×1"}
	if got := warnings(res); !slices.Equal(sortStrings(got), sortStrings(wantWarn)) {
		t.Errorf("warnings %q, want %q", got, wantWarn)
	}
	s := res.Session
	if s.GitBranch != "main" || s.StartedAt != 1790255252785 || s.SourceVersion != "0.156.1" {
		t.Errorf("session %+v", s)
	}
}

func sortStrings(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

// A Child Session's inherited parent context is Raw only; it links to its
// parent Session.
func TestChildSession(t *testing.T) {
	child := "01a0d390-1111-7222-8333-944455556666"
	b := build(t, 0,
		meta(child, map[string]any{"parent_thread_id": parent, "subagent_history_start_ordinal": 4,
			"source":         map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent}}},
			"agent_nickname": "Scout", "agent_role": "explorer"}),
		turn("gpt-6-sol", "medium"),
		user(inText("parent's prompt")),
		say("final_answer", "parent's answer"),
		turn("gpt-6-sol", "low"), // 4: the sub-agent's own first turn: no marker
		user(inText("Explore the repo")),
		say("final_answer", "Found it."),
	)
	res := parse(t, parser.Input{NativeID: child, MainKey: "rollout-2026-09-24T09-30-00-" + child + ".jsonl", Main: b})
	var got []string
	for _, m := range res.Messages {
		got = append(got, m.ID+":"+strings.Join(texts(m), ","))
	}
	if want := []string{"5:Explore the repo", "6:Found it."}; !slices.Equal(got, want) {
		t.Errorf("messages %q, want %q", got, want)
	}
	if res.Session.ParentNativeID != parent || res.Session.SpawningCallID != "" || res.Session.ForkedFromNativeID != "" {
		t.Errorf("session %+v", res.Session)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %v", warnings(res))
	}
}

func TestFork(t *testing.T) {
	b := build(t, 0, meta(thread, map[string]any{"forked_from_id": parent, "forked_from_ordinal_exclusive": 9}), user(inText("hi")))
	res := parse(t, parser.Input{Main: b})
	if res.Session.ForkedFromNativeID != parent || res.Session.ParentNativeID != "" {
		t.Errorf("session %+v", res.Session)
	}
}

// A thread with a continuation file parses as one Session: the continuation
// follows its history_base into the main file, cutting it where it
// continues (codex.md §3.2).
func TestContinuationStitching(t *testing.T) {
	main := build(t, 0,
		meta(thread, nil),
		turn("gpt-6-sol", "medium"),
		user(inText("one")),      // 2
		say("final_answer", "1"), // 3
		user(inText("two")),      // 4: reverted away
		say("final_answer", "2"), // 5: reverted away
	)
	cont := build(t, 4,
		meta(thread, map[string]any{"history_base": map[string]any{"thread_id": thread, "end_ordinal_exclusive": 4, "end_byte_offset": 1234}}),
		user(inText("two again")), // 5
		say("final_answer", "2b"), // 6
	)
	// An older revert, which the current file no longer continues.
	stale := "rollout-2026-09-24T09-50-00-" + thread + "_01a0d398-0000-7000-8000-000000000000.jsonl"
	staleBody := build(t, 4,
		meta(thread, map[string]any{"history_base": map[string]any{"thread_id": thread, "end_ordinal_exclusive": 4, "end_byte_offset": 1234}}),
		user(inText("stale")),
	)
	res := parse(t, parser.Input{Main: main, Attachments: map[string][]byte{contKey: cont, stale: staleBody}})
	var got []string
	for _, m := range res.Messages {
		got = append(got, m.ID+":"+strings.Join(texts(m), ","))
	}
	if want := []string{"2:one", "3:1", "5:two again", "6:2b"}; !slices.Equal(got, want) {
		t.Errorf("messages %q, want %q", got, want)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %v", warnings(res))
	}
	if res.Session.LastActivityAt <= res.Session.StartedAt {
		t.Errorf("session times %+v", res.Session)
	}
}

// Without history_base, files follow <ts> order, and a later file's line
// wins an ordinal both hold; its Message gets a hashed id.
func TestContinuationWithoutHistoryBase(t *testing.T) {
	main := build(t, 0, meta(thread, nil), user(inText("one")), say("final_answer", "1"))
	cont := build(t, 1, meta(thread, nil), say("final_answer", "1b"), user(inText("two")))
	res := parse(t, parser.Input{Main: main, Attachments: map[string][]byte{contKey: cont}})
	var got []string
	for _, m := range res.Messages {
		got = append(got, strings.Join(texts(m), ","))
	}
	if want := []string{"one", "1b", "two"}; !slices.Equal(got, want) {
		t.Fatalf("messages %q, want %q", got, want)
	}
	if id := res.Messages[1].ID; id == "2" || parser.SafeID(id) != id {
		t.Errorf("repeated ordinal's message id %q", id)
	}
	if res.Messages[0].ID != "1" || res.Messages[2].ID != "3" {
		t.Errorf("ids %q %q", res.Messages[0].ID, res.Messages[2].ID)
	}
}

// A thread whose key and session_meta disagree keeps the key's id.
func TestIDMismatch(t *testing.T) {
	res := parse(t, parser.Input{Main: build(t, 0, meta(parent, nil), user(inText("hi")))})
	if got := warnings(res); !slices.Equal(got, []string{"missing_field:id×1"}) {
		t.Errorf("warnings %q", got)
	}
}

func TestDeterministic(t *testing.T) {
	_, raw := fixture(t, "rollout-2026-09-14T12-32-34-01a0a0c3-6416-72d2-ab61-cb1873b7787e.jsonl")
	in := parser.Input{NativeID: parent, MainKey: "rollout-2026-09-14T12-32-34-" + parent + ".jsonl", Main: raw}
	a, b := parse(t, in), parse(t, in)
	if !reflect.DeepEqual(a, b) {
		t.Error("two parses differ")
	}
}

// Fixes from review: each case once broke a §3 rule.
func TestEdgeCases(t *testing.T) {
	t.Run("usage after an empty assistant message goes to the Message shown", func(t *testing.T) {
		res := parse(t, parser.Input{Main: build(t, 0,
			meta(thread, nil), turn("gpt-6-sol", "medium"), user(inText("hi")),
			say("commentary", "Working."),
			rl{"response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
			usage("resp_1", 10, 1),
		)})
		if m := res.Messages[1]; m.Usage == nil || m.Usage.Input != 10 {
			t.Errorf("usage %+v", m.Usage)
		}
	})
	t.Run("a second output for one call is an orphan", func(t *testing.T) {
		res := parse(t, parser.Input{Main: build(t, 0,
			meta(thread, nil), user(inText("hi")),
			rl{"response_item", map[string]any{"type": "function_call", "name": "shell", "arguments": "{}", "call_id": "c1"}},
			rl{"response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "first"}},
			rl{"response_item", map[string]any{"type": "function_call_output", "call_id": "c1", "output": "second"}},
		)})
		if got := warnings(res); !slices.Equal(got, []string{"orphan:function_call_output×1"}) {
			t.Errorf("warnings %q", got)
		}
		if tc := res.Messages[1].Parts[0].Payload.(parser.ToolCallPayload); *tc.Output != "first" {
			t.Errorf("output %q", *tc.Output)
		}
	})
	t.Run("injected context is one whole element", func(t *testing.T) {
		for text, want := range map[string]bool{
			"<environment_context>\n  <cwd>/x</cwd>\n</environment_context>": true,
			`<skill name="x">body</skill >`:                                  true,
			"<a>x</a> and then <a>y</a>":                                     false,
			"<b>bold</b> is what I mean":                                     false,
			"<unclosed> tag":                                                 false,
			"plain":                                                          false,
		} {
			if got := injected(text); got != want {
				t.Errorf("injected(%q) = %v, want %v", text, got, want)
			}
		}
	})
	t.Run("a Child Session takes its model from inherited context", func(t *testing.T) {
		child := "01a0d390-1111-7222-8333-944455556666"
		res := parse(t, parser.Input{NativeID: child, MainKey: "rollout-2026-09-24T09-30-00-" + child + ".jsonl", Main: build(t, 0,
			meta(child, map[string]any{"parent_thread_id": parent, "subagent_history_start_ordinal": 2}),
			turn("gpt-6-sol", "high"),
			user(inText("Explore")),
			say("final_answer", "Found it."),
		)})
		if len(res.Messages) != 2 || res.Messages[1].Model != "gpt-6-sol" || len(res.Messages[0].Parts) != 1 {
			t.Fatalf("messages %+v", res.Messages)
		}
	})
	t.Run("a continuation whose chain misses the main file follows <ts>", func(t *testing.T) {
		main := build(t, 0, meta(thread, nil), user(inText("one")))
		cont := build(t, 2, meta(thread, map[string]any{"history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2}}), user(inText("two")))
		res := parse(t, parser.Input{Main: main, Attachments: map[string][]byte{contKey: cont}})
		var got []string
		for _, m := range res.Messages {
			got = append(got, strings.Join(texts(m), ","))
		}
		if !slices.Equal(got, []string{"one", "two"}) {
			t.Errorf("messages %q", got)
		}
	})
	t.Run("a history_base cycle doesn't panic", func(t *testing.T) {
		main := build(t, 0, meta(thread, map[string]any{"history_base": map[string]any{"thread_id": rollout, "end_ordinal_exclusive": 1}}), user(inText("one")))
		cont := build(t, 2, meta(thread, map[string]any{"history_base": map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2}}), user(inText("two")))
		res := parse(t, parser.Input{Main: main, Attachments: map[string][]byte{contKey: cont}})
		if len(res.Messages) != 2 {
			t.Errorf("messages %+v", res.Messages)
		}
	})
}
