package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	sessA = "3d34bfcc-90e7-4fd0-900f-86c047b22433"
	sessB = "7a1e0c55-2b3d-4c8e-9f10-112233445566"
	sessC = "0b9d8e7f-6a5b-4c3d-8e2f-aabbccddeeff"
)

// jsonLines joins Claude Code records, one per line.
func jsonLines(lines ...map[string]any) string {
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(append(j, '\n'))
	}
	return b.String()
}

func userLine(uuid, parent, at, text string) map[string]any {
	l := map[string]any{"type": "user", "uuid": uuid, "parentUuid": parent, "timestamp": at, "cwd": "/Users/ted/src/app",
		"message": map[string]any{"role": "user", "content": text}}
	if parent == "" {
		l["parentUuid"] = nil
	}
	return l
}

func assistantLine(uuid, parent, at string, content ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "uuid": uuid, "parentUuid": parent, "timestamp": at,
		"message": map[string]any{"id": "msg_" + uuid, "model": "m", "content": content}}
}

func resultLine(uuid, parent, at, callID, output, agent string) map[string]any {
	l := map[string]any{"type": "user", "uuid": uuid, "parentUuid": parent, "timestamp": at,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output}}}}
	if agent != "" {
		l["toolUseResult"] = map[string]any{"agentId": agent}
	}
	return l
}

func toolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// bigOutput is over the 16 KB the Hub keeps in a tool call's payload.
var bigOutput = strings.Repeat("0123456789abcdef", 1200) + "THE END"

// fixture is three Claude Code Sessions:
//   - A on laptop, 2026-09-01, in /Users/ted/src/app: a prompt about the
//     sqlite lock, a reply with thinking, an Agent call spawning Child
//     Session "a", and a Bash call with output too big for its payload.
//   - B on desk, 2026-09-20, also in /Users/ted/src/app: one short exchange.
//   - C on laptop, 2026-09-10, in /Users/ted/src/lib: 220 Messages, more
//     than any one call returns.
func fixture() map[string][2]string {
	a := jsonLines(
		userLine("u1", "", "2026-09-01T10:00:00.000Z", "how do I fix the sqlite lock?"),
		assistantLine("a1", "u1", "2026-09-01T10:00:01.000Z",
			map[string]any{"type": "thinking", "thinking": "consider busy_timeout"},
			map[string]any{"type": "text", "text": "Set busy_timeout on the connection."}),
		assistantLine("a2", "a1", "2026-09-01T10:00:02.000Z", toolUse("t1", "Agent", map[string]any{"description": "Explore the store", "prompt": "look"})),
		resultLine("r1", "a2", "2026-09-01T10:00:03.000Z", "t1", "explored", "a"),
		assistantLine("a3", "r1", "2026-09-01T10:00:04.000Z", toolUse("t2", "Bash", map[string]any{"command": "go test ./..."})),
		resultLine("r2", "a3", "2026-09-01T10:00:05.000Z", "t2", bigOutput, ""),
		map[string]any{"type": "custom-title", "customTitle": "Fix the sqlite lock"},
	)
	child := jsonLines(map[string]any{"type": "user", "uuid": "c1", "parentUuid": nil, "isSidechain": true,
		"timestamp": "2026-09-01T10:00:02.500Z", "cwd": "/Users/ted/src/app",
		"message": map[string]any{"role": "user", "content": "explore the sqlite store"}})
	shot := userLine("u2", "a1", "2026-09-20T09:00:02.000Z", "")
	shot["message"] = map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "see the screenshot"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
	}}
	b := jsonLines(
		userLine("u1", "", "2026-09-20T09:00:00.000Z", "sqlite vacuum on desk"),
		assistantLine("a1", "u1", "2026-09-20T09:00:01.000Z", map[string]any{"type": "text", "text": "Run VACUUM."}),
		shot,
	)
	var c []map[string]any
	parent := ""
	for i := range 220 {
		id := fmt.Sprintf("m%02d", i)
		at := fmt.Sprintf("2026-09-10T%02d:%02d:00.000Z", 8+i/60, i%60)
		if i%2 == 0 {
			c = append(c, userLine(id, parent, at, fmt.Sprintf("question %d", i)))
		} else {
			c = append(c, assistantLine(id, parent, at, map[string]any{"type": "text", "text": fmt.Sprintf("answer %d", i)}))
		}
		parent = id
	}
	for _, l := range c {
		l["cwd"] = "/Users/ted/src/lib"
	}
	return map[string][2]string{
		"-Users-ted-src-app/" + sessA + ".jsonl":                   {"m1", a},
		"-Users-ted-src-app/" + sessA + "/subagents/agent-a.jsonl": {"m1", child},
		"-Users-ted-src-app/" + sessB + ".jsonl":                   {"m2", b},
		"-Users-ted-src-lib/" + sessC + ".jsonl":                   {"m1", jsonLines(c...)},
	}
}

// newStore is a store holding the fixture, parsed. Machine m1 is "laptop"
// with home /Users/ted; m2 is "desk" with home /home/ted.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	reg := parser.NewRegistry(claudecode.New())
	s, err := store.Open(ctx, t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertMachine(ctx, "m1", protocol.MachineInfo{Hostname: "laptop", HomeDir: "/Users/ted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMachine(ctx, "m2", protocol.MachineInfo{Hostname: "desk", HomeDir: "/home/ted"}); err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	empty := sha256.Sum256(nil)
	for key, r := range fixture() {
		if _, err := s.Append(ctx, store.AppendRequest{
			MachineID: r[0], Source: protocol.SourceClaudeCode, RecordKey: key,
			PrefixSha256: hex.EncodeToString(empty[:]), Data: []byte(r[1]), Compressed: enc.EncodeAll([]byte(r[1]), nil),
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := worker.New(s, reg, nil)
	for {
		worked, _, err := w.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	return s
}

// req is a tool call request arriving at http://hub:8080.
var req = &gomcp.CallToolRequest{Extra: &gomcp.RequestExtra{Header: http.Header{baseHeader: {"http://hub:8080"}}}}

func newTools(t *testing.T) *tools {
	return &tools{store: newStore(t), log: slog.Default()}
}

func search(t *testing.T, tl *tools, in SearchInput) SearchOutput {
	t.Helper()
	_, out, err := tl.search(context.Background(), req, in)
	if err != nil {
		t.Fatalf("search(%+v): %v", in, err)
	}
	return out
}

func list(t *testing.T, tl *tools, in ListInput) ListOutput {
	t.Helper()
	_, out, err := tl.listSessions(context.Background(), req, in)
	if err != nil {
		t.Fatalf("list_sessions(%+v): %v", in, err)
	}
	return out
}

func transcript(t *testing.T, tl *tools, in TranscriptInput) TranscriptOutput {
	t.Helper()
	_, out, err := tl.getTranscript(context.Background(), req, in)
	if err != nil {
		t.Fatalf("get_transcript(%+v): %v", in, err)
	}
	return out
}

func natives(ss []Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.NativeID)
	}
	return out
}

func TestSearchQuotesHitsWithFacets(t *testing.T) {
	tl := newTools(t)
	out := search(t, tl, SearchInput{Query: "sqlite"})
	if out.Note != quoted {
		t.Errorf("note = %q", out.Note)
	}
	var u1 *Hit
	for i, h := range out.Hits {
		if h.NativeID == sessA && h.MessageID == "u1" {
			u1 = &out.Hits[i]
		}
	}
	if u1 == nil {
		t.Fatalf("no hit on A's prompt in %+v", out.Hits)
	}
	if u1.Title != "Fix the sqlite lock" || u1.Machine != "laptop" || u1.Source != "claude-code" || u1.Project != "/Users/ted/src/app" ||
		u1.Role != "user" || !strings.HasPrefix(u1.Date, "2026-09-01") || !strings.Contains(u1.Snippet, "**sqlite**") ||
		u1.URL != fmt.Sprintf("http://hub:8080/sessions/%d#m-u1", u1.SessionID) {
		t.Errorf("hit = %+v", *u1)
	}

	// The Child Session's hit names its parent.
	var child *Hit
	for i, h := range out.Hits {
		if h.Parent != nil {
			child = &out.Hits[i]
		}
	}
	if child == nil || child.Parent.SessionID != u1.SessionID || child.Parent.Title != "Fix the sqlite lock" {
		t.Errorf("child hit = %+v", child)
	}

	if !slices.Contains(out.Facets.Machines, Facet{Value: "desk", Hits: 2}) {
		t.Errorf("machine facets = %+v", out.Facets.Machines)
	}
	if !slices.ContainsFunc(out.Facets.Projects, func(f Facet) bool { return f.Value == "/Users/ted/src/app" && f.Machine == "desk" }) {
		t.Errorf("project facets = %+v", out.Facets.Projects)
	}
}

func TestSearchFilters(t *testing.T) {
	tl := newTools(t)
	sessions := func(in SearchInput) []string {
		var out []string
		for _, h := range search(t, tl, in).Hits {
			if !slices.Contains(out, h.NativeID) {
				out = append(out, h.NativeID)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		name string
		in   SearchInput
		want []string
	}{
		{"machine by name", SearchInput{Query: "sqlite", Filters: Filters{Machine: "DESK"}}, []string{sessB}},
		{"machine by id", SearchInput{Query: "sqlite", Filters: Filters{Machine: "m2"}}, []string{sessB}},
		{"source", SearchInput{Query: "sqlite", Filters: Filters{Source: "codex"}}, nil},
		// The same path on two Machines is two Projects; cwd matches both.
		{"cwd on any machine", SearchInput{Query: "question", Filters: Filters{Cwd: "/Users/ted/src/lib/"}}, []string{sessC}},
		{"since", SearchInput{Query: "sqlite", Filters: Filters{Since: "2026-09-15"}}, []string{sessB}},
		{"until includes the day", SearchInput{Query: "sqlite vacuum", Filters: Filters{Until: "2026-09-20"}}, []string{sessB}},
		{"until rfc3339", SearchInput{Query: "sqlite vacuum", Filters: Filters{Until: "2026-09-20T00:00:00Z"}}, nil},
	} {
		if got := sessions(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s: Sessions %v, want %v", c.name, got, c.want)
		}
	}
	both := sessions(SearchInput{Query: "sqlite", Filters: Filters{Cwd: "/Users/ted/src/app"}})
	if !slices.Contains(both, sessA) || !slices.Contains(both, sessB) {
		t.Errorf("cwd across Machines = %v", both)
	}

	if n := len(search(t, tl, SearchInput{Query: "question", Filters: Filters{Limit: 3}}).Hits); n != 3 {
		t.Errorf("limit 3: %d hits", n)
	}
	// 110 questions and C's title match, but one call returns at most 100.
	if n := len(search(t, tl, SearchInput{Query: "question", Filters: Filters{Limit: 500}}).Hits); n != 100 {
		t.Errorf("limit 500: %d hits, want the cap of 100", n)
	}
	if n := len(search(t, tl, SearchInput{Query: "question"}).Hits); n != 20 {
		t.Errorf("default limit: %d hits, want 20", n)
	}

	for _, bad := range []SearchInput{
		{Query: "  \"\" "},
		{Query: "sqlite", Filters: Filters{Machine: "nowhere"}},
		{Query: "sqlite", Filters: Filters{Since: "last week"}},
	} {
		if _, _, err := tl.search(context.Background(), req, bad); err == nil {
			t.Errorf("search(%+v) has no error", bad)
		}
	}
	_, _, err := tl.search(context.Background(), req, SearchInput{Query: "x", Filters: Filters{Machine: "nowhere"}})
	if err == nil || !strings.Contains(err.Error(), "desk") || !strings.Contains(err.Error(), "laptop") {
		t.Errorf("unknown Machine error = %v; want it to name the Machines", err)
	}
}

func TestListSessionsNewestFirstAndPages(t *testing.T) {
	tl := newTools(t)
	out := list(t, tl, ListInput{})
	if got, want := natives(out.Sessions), []string{sessB, sessC, sessA}; !slices.Equal(got, want) {
		t.Fatalf("sessions = %v, want %v (no Child Session)", got, want)
	}
	if out.Next != "" {
		t.Errorf("next on the only page = %q", out.Next)
	}
	if s := out.Sessions[0]; s.Machine != "desk" || s.Title != "sqlite vacuum on desk" || !strings.HasPrefix(s.Date, "2026-09-20") ||
		s.URL != fmt.Sprintf("http://hub:8080/sessions/%d", s.SessionID) {
		t.Errorf("session = %+v", s)
	}

	var paged []string
	in := ListInput{Filters: Filters{Limit: 2}}
	for range 3 {
		page := list(t, tl, in)
		paged = append(paged, natives(page.Sessions)...)
		if page.Next == "" {
			break
		}
		in.Before = page.Next
	}
	if want := []string{sessB, sessC, sessA}; !slices.Equal(paged, want) {
		t.Errorf("paged = %v, want %v", paged, want)
	}

	if got := natives(list(t, tl, ListInput{Filters: Filters{Since: "2026-09-05", Until: "2026-09-15"}}).Sessions); !slices.Equal(got, []string{sessC}) {
		t.Errorf("since/until = %v, want only C", got)
	}
	if got := natives(list(t, tl, ListInput{Filters: Filters{Machine: "laptop", Cwd: "/Users/ted/src/app"}}).Sessions); !slices.Equal(got, []string{sessA}) {
		t.Errorf("machine+cwd = %v", got)
	}
	if _, _, err := tl.listSessions(context.Background(), req, ListInput{Before: "yesterday"}); err == nil {
		t.Error("a bad before has no error")
	}
}

func TestGetTranscriptAroundAHit(t *testing.T) {
	tl := newTools(t)
	var a, c int64
	for _, s := range list(t, tl, ListInput{}).Sessions {
		switch s.NativeID {
		case sessA:
			a = s.SessionID
		case sessC:
			c = s.SessionID
		}
	}

	out := transcript(t, tl, TranscriptInput{SessionID: c, Around: "m10", Context: 2})
	var ids []string
	for _, m := range out.Messages {
		ids = append(ids, m.MessageID)
	}
	if want := []string{"m08", "m09", "m10", "m11", "m12"}; !slices.Equal(ids, want) {
		t.Errorf("around m10 ±2 = %v, want %v", ids, want)
	}
	if out.Messages[2].Position != 10 || out.Messages[2].Content != "question 10" || out.Messages[2].Role != "user" {
		t.Errorf("m10 = %+v", out.Messages[2])
	}
	if out.Total != 220 || out.Next == nil || *out.Next != 13 {
		t.Errorf("total %d, next %v", out.Total, out.Next)
	}
	if out.Session.Title != "question 0" || out.Session.Machine != "laptop" || out.Note != quoted {
		t.Errorf("session = %+v", out.Session)
	}
	// Near the start, the window is cut, not shifted; context is capped at 50.
	if out := transcript(t, tl, TranscriptInput{SessionID: c, Around: "m01", Context: 80}); len(out.Messages) != 52 || out.Next == nil || *out.Next != 52 {
		t.Errorf("around m01 ±50 = %d Messages, next %v; want m00 to m51", len(out.Messages), out.Next)
	}
	if out := transcript(t, tl, TranscriptInput{SessionID: c, Around: "m100", Context: 80}); len(out.Messages) != 101 || out.Messages[0].MessageID != "m50" {
		t.Errorf("around m100 ±50 = %d Messages from %s", len(out.Messages), out.Messages[0].MessageID)
	}

	// Images are a placeholder line.
	var b int64
	for _, s := range list(t, tl, ListInput{}).Sessions {
		if s.NativeID == sessB {
			b = s.SessionID
		}
	}
	if got := contents(transcript(t, tl, TranscriptInput{SessionID: b})); !strings.HasSuffix(got, "see the screenshot\n[image]") {
		t.Errorf("B = %q, want the image as a placeholder", got)
	}

	// A's Transcript: thinking and output are left out unless asked for.
	out = transcript(t, tl, TranscriptInput{SessionID: a, Around: "a2", Context: 50})
	text := contents(out)
	for _, want := range []string{"how do I fix the sqlite lock?", "Set busy_timeout on the connection.",
		"[tool call] Agent: Explore the store", "[tool call] Bash: go test ./..."} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"busy_timeout\n[/thinking]", "consider busy_timeout", "0123456789abcdef"} {
		if strings.Contains(text, bad) {
			t.Errorf("transcript has %q unasked:\n%s", bad, text)
		}
	}
	// The spawning call names its Child Session, whose Transcript names its parent.
	var childID int64
	if _, err := fmt.Sscanf(text[strings.Index(text, "[spawned Child Session ")+len("[spawned Child Session "):], "%d", &childID); err != nil {
		t.Fatalf("no Child Session on the Agent call:\n%s", text)
	}
	child := transcript(t, tl, TranscriptInput{SessionID: childID, From: new(0)})
	if child.Parent == nil || child.Parent.SessionID != a || contents(child) != "explore the sqlite store" {
		t.Errorf("child = %+v", child)
	}

	out = transcript(t, tl, TranscriptInput{SessionID: a, From: new(0), IncludeThinking: true, IncludeToolOutput: true})
	text = contents(out)
	if !strings.Contains(text, "[thinking]\nconsider busy_timeout\n[/thinking]") || !strings.Contains(text, "[output]\nexplored\n[/output]") {
		t.Errorf("thinking or small output missing:\n%s", text)
	}
	// Big output is read from where the Hub split it to, then cut to 2 KB.
	i := strings.Index(text, "[output]\n0123456789abcdef")
	if i < 0 || !strings.Contains(text, fmt.Sprintf("… %d more bytes", len(bigOutput)-toolOutputMax)) || strings.Contains(text, "THE END") {
		t.Errorf("big output not cut to 2 KB:\n%s", text)
	}
}

func contents(out TranscriptOutput) string {
	var parts []string
	for _, m := range out.Messages {
		if m.Content != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func TestGetTranscriptPages(t *testing.T) {
	tl := newTools(t)
	c := list(t, tl, ListInput{Filters: Filters{Cwd: "/Users/ted/src/lib"}}).Sessions[0].SessionID
	var ids []string
	in := TranscriptInput{SessionID: c, From: new(0), Limit: 90}
	for range 5 {
		out := transcript(t, tl, in)
		for _, m := range out.Messages {
			ids = append(ids, m.MessageID)
		}
		if out.Next == nil {
			break
		}
		in.From = out.Next
	}
	if len(ids) != 220 || ids[0] != "m00" || ids[219] != "m219" {
		t.Errorf("paged %d ids: %v … %v", len(ids), ids[:3], ids[len(ids)-3:])
	}
	if out := transcript(t, tl, TranscriptInput{SessionID: c}); len(out.Messages) != 50 || *out.Next != 50 {
		t.Errorf("default page = %d Messages", len(out.Messages))
	}
	if out := transcript(t, tl, TranscriptInput{SessionID: c, Limit: 7}); len(out.Messages) != 7 {
		t.Errorf("limit 7 without from = %d Messages", len(out.Messages))
	}
	if out := transcript(t, tl, TranscriptInput{SessionID: c, From: new(0), Limit: 1000}); len(out.Messages) != 100 {
		t.Errorf("limit 1000 = %d Messages, want the cap of 100", len(out.Messages))
	}

	for _, bad := range []TranscriptInput{
		{SessionID: c, Around: "m01", From: new(3)},
		{SessionID: c, Around: "nope"},
		{SessionID: 9999},
	} {
		if _, _, err := tl.getTranscript(context.Background(), req, bad); err == nil {
			t.Errorf("get_transcript(%+v) has no error", bad)
		}
	}
}

// TestEndToEnd drives /mcp with the go-sdk client the way an agent would:
// list the tools, search, then read around the hit.
func TestEndToEnd(t *testing.T) {
	st := newStore(t)
	before := snapshot(t, st)
	mux := http.NewServeMux()
	mux.Handle(Path, New(st, "", nil))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &gomcp.StreamableClientTransport{Endpoint: srv.URL + Path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tl.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s isn't marked read-only", tool.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"get_transcript", "list_sessions", "search"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	var found SearchOutput
	call(t, cs, "search", map[string]any{"query": "busy_timeout"}, &found)
	if len(found.Hits) != 1 || found.Hits[0].MessageID != "a1" {
		t.Fatalf("hits = %+v", found.Hits)
	}
	hit := found.Hits[0]
	// Links are made from the request's Host.
	if want := srv.URL + fmt.Sprintf("/sessions/%d#m-a1", hit.SessionID); hit.URL != want {
		t.Errorf("url = %q, want %q", hit.URL, want)
	}
	var around TranscriptOutput
	call(t, cs, "get_transcript", map[string]any{"session_id": hit.SessionID, "around": hit.MessageID, "context": 1}, &around)
	var ids []string
	for _, m := range around.Messages {
		ids = append(ids, m.MessageID)
	}
	if want := []string{"u1", "a1", "a2"}; !slices.Equal(ids, want) {
		t.Errorf("around a1 = %v, want %v", ids, want)
	}

	var recent ListOutput
	call(t, cs, "list_sessions", map[string]any{"since": "2026-09-15"}, &recent)
	if got := natives(recent.Sessions); !slices.Equal(got, []string{sessB}) {
		t.Errorf("list_sessions since = %v", got)
	}

	res, err := cs.CallTool(ctx, &gomcp.CallToolParams{Name: "get_transcript", Arguments: map[string]any{"session_id": 9999}})
	if err != nil || !res.IsError {
		t.Errorf("unknown Session = %+v, %v; want a tool error", res, err)
	}

	// Mutating tools don't exist, and nothing the calls did changed the data.
	if _, err := cs.CallTool(ctx, &gomcp.CallToolParams{Name: "reparse", Arguments: map[string]any{}}); err == nil {
		t.Error("an unknown tool was called")
	}
	if after := snapshot(t, st); after != before {
		t.Errorf("stored data changed:\nbefore %s\nafter  %s", before, after)
	}

	// AGENT_HISTORY_PUBLIC_URL wins over the Host.
	pub := httptest.NewServer(New(st, "https://history.example.com", nil))
	t.Cleanup(pub.Close)
	cs2, err := client.Connect(ctx, &gomcp.StreamableClientTransport{Endpoint: pub.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	call(t, cs2, "search", map[string]any{"query": "busy_timeout"}, &found)
	if !strings.HasPrefix(found.Hits[0].URL, "https://history.example.com/sessions/") {
		t.Errorf("url with public URL = %q", found.Hits[0].URL)
	}
}

func call(t *testing.T, cs *gomcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error %+v", name, res.Content)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// snapshot sums what the tools could change: the Sessions, Messages and the
// parse queue.
func snapshot(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	rows, err := st.Feed(ctx, store.FeedFilter{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		_, msgs, total, _, err := st.TranscriptRange(ctx, r.ID, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%d:%d:%d:%s;", r.ID, r.LastActivityAt, total, msgs[len(msgs)-1].ID)
	}
	return b.String()
}
