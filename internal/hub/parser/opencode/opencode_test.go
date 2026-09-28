package opencode

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// The fixtures have the row and file shapes of opencode 1.18.31 (sqlite)
// and 0.4.45 (legacy-json), with invented content.
const (
	parentID = "ses_0575ede0fffeQx7cLs3vMw9nTp"
	childID  = "ses_0575dd36bffeNw8xMq2vLs4cTp"
	legacyID = "ses_2d1b8f2a3ffeQm7xLs9vNw4cTp"
	lChildID = "ses_2d1b8e1c4ffeNw2xMq8vLs5cTp"
	proj     = "0123456789abcdef0123456789abcdef01234567"
)

func TestMapKey(t *testing.T) {
	m := func(id, role, layout string, rank int) parser.Mapping {
		return parser.Mapping{NativeID: id, Role: role, Layout: layout, LayoutRank: rank}
	}
	none := parser.Mapping{}
	for _, c := range []struct {
		key  string
		want parser.Mapping
		ok   bool
	}{
		{"db:ses_abc", m("ses_abc", parser.RoleMain, "sqlite", 2), true},
		{"json:session/proj/ses_abc.json", m("ses_abc", parser.RoleMain, "legacy-json", 1), true},
		{"json:message/ses_abc/msg_1.json", m("ses_abc", parser.RoleAttachment, "legacy-json", 1), true},
		{"json:part/ses_abc/msg_1/prt_1.json", m("ses_abc", parser.RoleAttachment, "legacy-json", 1), true},
		{"json:part/_/msg_1/prt_1.json", none, false},
		{"db:abc", none, false},
		{"db:ses_", none, false},
		{"json:session/proj/abc.json", none, false},
		{"json:session/ses_abc.json", none, false},
		{"json:message/ses_abc/prt_1.json", none, false},
		{"json:part/ses_abc/msg_1/prt_1.txt", none, false},
		{"json:session_diff/ses_abc.json", none, false},
		{"session/proj/ses_abc.json", none, false},
	} {
		got, ok := New().MapKey(c.key)
		if ok != c.ok || got != c.want {
			t.Errorf("MapKey(%q) = %+v, %v; want %+v, %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

func read(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseDB(t *testing.T, id string) parser.Result {
	t.Helper()
	return parse(t, parser.Input{NativeID: id, MainKey: "db:" + id, Main: read(t, "sqlite/"+id+".jsonl")})
}

func parse(t *testing.T, in parser.Input) parser.Result {
	t.Helper()
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

// legacyInput loads a legacy Session the way the Hub hands it over: the
// session file as Main, its message and part files as attachments.
func legacyInput(t *testing.T, id string) parser.Input {
	t.Helper()
	in := parser.Input{NativeID: id, Attachments: map[string][]byte{}}
	root := filepath.Join("testdata", "legacy", "storage")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		key := "json:" + filepath.ToSlash(rel)
		if strings.HasPrefix(key, "json:part/") {
			// The Collector inserts the part's Session.
			var pt struct{ SessionID string }
			b, _ := os.ReadFile(p)
			json.Unmarshal(b, &pt)
			key = "json:part/" + pt.SessionID + "/" + strings.TrimPrefix(key, "json:part/")
		}
		m, ok := New().MapKey(key)
		if !ok || m.NativeID != id {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if m.Role == parser.RoleMain {
			in.MainKey, in.Main = key, b
		} else {
			in.Attachments[key] = b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func texts(m parser.Message) []string {
	var out []string
	for _, p := range m.Parts {
		out = append(out, p.Kind+":"+summary(p))
	}
	return out
}

func summary(p parser.Part) string {
	switch v := p.Payload.(type) {
	case parser.TextPayload:
		return v.Text
	case parser.ThinkingPayload:
		return v.Text
	case parser.AttachmentPayload:
		return v.Label
	case parser.MarkerPayload:
		return v.Marker + "=" + v.Text
	case parser.ToolCallPayload:
		return v.Name + "/" + v.Status
	case parser.ImagePayload:
		return v.MIME
	case parser.UnknownPayload:
		return v.SourceType
	}
	return "?"
}

func TestSqliteSession(t *testing.T) {
	res := parseDB(t, parentID)
	want := parser.Session{
		Title: "Implement the design discussion", StartedAt: 1785240560112, LastActivityAt: 1785240621178,
		Cwd: "/Users/ted/src/app", SourceVersion: "1.18.31",
	}
	if res.Session != want {
		t.Errorf("session = %+v\nwant %+v", res.Session, want)
	}

	var got [][]string
	for _, m := range res.Messages {
		got = append(got, append([]string{m.Role + " " + m.ID}, texts(m)...))
	}
	wantMsgs := [][]string{
		{"user msg_fa8a12208001Ro578HocMvjs4p", "text:Implement the design from the discussion.", "attachment:docs/design.md"},
		{"assistant msg_fa8a1221c001YkJ2j0cnEpwZVt", "thinking:The design names two changes; start by fetching the discussion.", "tool_call:webfetch/ok", "tool_call:bash/ok"},
		{"assistant msg_fa8a1286e001CjLr0Qx6WbN3aE", "text:I'll make the first change, then hand the survey to an explorer.", "tool_call:edit/ok", "tool_call:task/ok"},
		{"assistant msg_fa8a12a01001Wm4sPq7tNz1cXv", "tool_call:question/error", "tool_call:grep/pending", "text:Error: MessageAbortedError: Aborted"},
		{"user msg_fa8a12b01001Hs6wQn3vLc8tRx", "text:Here is the screenshot.", "image:image/png"},
		{"assistant msg_fa8a12c01001Rw2tMq8xVn4cLs", "attachment:patch: 2 files", "attachment:snapshot", "attachment:@explore", "marker:compaction=" + parser.CompactionText, "unknown:hologram"},
	}
	if !reflect.DeepEqual(got, wantMsgs) {
		t.Errorf("messages:\n%q\nwant\n%q", got, wantMsgs)
	}

	a := res.Messages[1]
	if a.Model != "kimi-k3" || a.Provider != "opencode-go" || a.Timestamp != 1785240560156 ||
		*a.Usage != (parser.Usage{Input: 11298, Output: 240, Reasoning: 192, CacheRead: 1024}) {
		t.Errorf("assistant = %+v, usage %+v", a, a.Usage)
	}
	if res.Messages[0].Usage != nil || res.Messages[0].Model != "" {
		t.Errorf("user message has model or usage: %+v", res.Messages[0])
	}
	if a.Parts[1].ID != "prt_fa8a12362001r4Yk7PzXe0HhQn" {
		t.Errorf("part id = %s", a.Parts[1].ID)
	}
	if id := res.Messages[3].Parts[2].ID; id != "msg_fa8a12a01001Wm4sPq7tNz1cXv.2" {
		t.Errorf("error part id = %s", id)
	}

	tool := func(m, p int) parser.ToolCallPayload {
		return res.Messages[m].Parts[p].Payload.(parser.ToolCallPayload)
	}
	bash := tool(1, 2)
	if bash.CallID != "call_01_Hv4XnP9sQe2B" || string(bash.Input) != `{"command":"go test ./...","description":"Run the tests"}` ||
		*bash.Output != "ok  \texample.com/app\t0.012s\n" || bash.Diff != nil || len(bash.ChildSessions) != 0 {
		t.Errorf("bash = %+v", bash)
	}
	edit := tool(2, 1)
	if edit.Diff == nil || *edit.Diff != (parser.Diff{Path: "/Users/ted/src/app/main.go", Old: `fmt.Println("hi")`, New: `fmt.Println("hello")`}) {
		t.Errorf("edit diff = %+v", edit.Diff)
	}
	if task := tool(2, 2); !reflect.DeepEqual(task.ChildSessions, []string{childID}) {
		t.Errorf("task child sessions = %q", task.ChildSessions)
	}
	if q := tool(3, 0); *q.Output != "The user dismissed this question" {
		t.Errorf("errored call output = %q", *q.Output)
	}
	if g := tool(3, 1); g.Output != nil {
		t.Errorf("running call has output %q", *g.Output)
	}
	if len(res.Images) != 1 || res.Images[0].MIME != "image/png" || len(res.Images[0].Bytes) == 0 {
		t.Errorf("images = %+v", res.Images)
	}
	if img := res.Messages[4].Parts[1].Payload.(parser.ImagePayload); img.SHA256 != res.Images[0].SHA256 || img.Alt != "shot.png" {
		t.Errorf("image part = %+v", img)
	}

	// The unknown part type is a warning; the reverted tail, the
	// synthetic text, step bookkeeping, session_message and the unknown
	// table are Raw only, silently.
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnUnknownType || res.Warnings[0].SourceType != "hologram" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	for _, m := range res.Messages {
		for _, s := range texts(m) {
			if strings.Contains(s, "drop the second change") || strings.Contains(s, "Dropped") || strings.Contains(s, "Called the Read tool") {
				t.Errorf("shows Raw-only text %q", s)
			}
		}
	}
}

func TestSqliteChildSession(t *testing.T) {
	res := parseDB(t, childID)
	if res.Session.ParentNativeID != parentID || res.Session.SpawningCallID != "" ||
		res.Session.Title != "Explore the remote architecture (@explore subagent)" {
		t.Errorf("session = %+v", res.Session)
	}
	if n := len(res.Messages); n != 3 {
		t.Fatalf("%d messages", n)
	}
	// A compaction summary's text is its marker.
	if got := texts(res.Messages[2]); !reflect.DeepEqual(got, []string{"marker:compaction=Summary: the client forwards every call to the server."}) {
		t.Errorf("summary message = %q", got)
	}
}

// A line that isn't JSON, or whose data isn't, is a bad_line warning; the
// rest still parses.
func TestSqliteBadLines(t *testing.T) {
	b := read(t, "sqlite/"+childID+".jsonl")
	b = append([]byte("not json\n"), b...)
	b = append(b, []byte(`{"table":"part","row":{"id":"prt_z","message_id":"msg_fa8a12880001Vc4nLq8wMs2xTp","data":"{oops"}}`+"\n")...)
	b = append(b, []byte(`{"table":"part","row":{"id":"prt_y","message_id":"msg_missing","data":"{\"type\":\"text\",\"text\":\"x\"}"}}`+"\n")...)
	res := parse(t, parser.Input{NativeID: childID, MainKey: "db:" + childID, Main: b})
	var kinds []string
	for _, w := range res.Warnings {
		kinds = append(kinds, w.Kind+"/"+w.SourceType)
	}
	if want := []string{"bad_line/", "bad_line/part", "orphan/part"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("warnings = %q, want %q", kinds, want)
	}
	if len(res.Messages) != 3 {
		t.Errorf("%d messages", len(res.Messages))
	}
}

func TestLegacySession(t *testing.T) {
	in := legacyInput(t, legacyID)
	if in.MainKey != "json:session/"+proj+"/"+legacyID+".json" || len(in.Attachments) != 8 {
		t.Fatalf("input: main %q, %d attachments", in.MainKey, len(in.Attachments))
	}
	res := parse(t, in)
	want := parser.Session{Title: "Fix the flaky test", StartedAt: 1755177596535, LastActivityAt: 1755177600535,
		Cwd: "/Users/ted/src/app", SourceVersion: "0.4.45"}
	if res.Session != want {
		t.Errorf("session = %+v", res.Session)
	}
	var got [][]string
	for _, m := range res.Messages {
		got = append(got, append([]string{m.Role}, texts(m)...))
	}
	wantMsgs := [][]string{
		{"user", "text:The login test fails one run in ten."},
		{"assistant", "tool_call:task/ok", "text:The test reads the real clock; I'll freeze it."},
	}
	if !reflect.DeepEqual(got, wantMsgs) {
		t.Errorf("messages = %q", got)
	}
	a := res.Messages[1]
	if a.Model != "claude-sonnet-4" || *a.Usage != (parser.Usage{Input: 12, Output: 340, CacheRead: 9000, CacheWrite: 800}) {
		t.Errorf("assistant = %+v %+v", a, a.Usage)
	}
	if task := a.Parts[0].Payload.(parser.ToolCallPayload); !reflect.DeepEqual(task.ChildSessions, []string{lChildID}) {
		t.Errorf("task children = %q", task.ChildSessions)
	}
	// The part whose message is gone.
	if len(res.Warnings) != 1 || res.Warnings[0].Kind != parser.WarnOrphan {
		t.Errorf("warnings = %+v", res.Warnings)
	}

	child := parse(t, legacyInput(t, lChildID))
	if child.Session.ParentNativeID != legacyID || len(child.Messages) != 0 {
		t.Errorf("child = %+v", child)
	}
	// The oldest session files have no directory.
	var kinds []string
	for _, w := range child.Warnings {
		kinds = append(kinds, w.Kind+"/"+w.SourceType)
	}
	if !reflect.DeepEqual(kinds, []string{"missing_field/directory"}) {
		t.Errorf("child warnings = %q", kinds)
	}
}

// Both Layouts parse the same objects the same way: the sqlite fixture's
// rows, written out as legacy files, give the same Transcript.
func TestLayoutsAgree(t *testing.T) {
	db := parseDB(t, childID)
	in := parser.Input{NativeID: childID, Attachments: map[string][]byte{}}
	for _, line := range strings.Split(strings.TrimSpace(string(read(t, "sqlite/"+childID+".jsonl"))), "\n") {
		var l struct {
			Table string
			Row   map[string]any
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatal(err)
		}
		r := l.Row
		switch l.Table {
		case "session":
			s, _ := json.Marshal(map[string]any{"id": r["id"], "title": r["title"], "version": r["version"], "directory": r["directory"],
				"parentID": r["parent_id"], "time": map[string]any{"created": r["time_created"], "updated": r["time_updated"]}})
			in.MainKey, in.Main = "json:session/"+proj+"/"+childID+".json", s
		case "message":
			in.Attachments["json:message/"+childID+"/"+r["id"].(string)+".json"] = []byte(r["data"].(string))
		case "part":
			in.Attachments["json:part/"+childID+"/"+r["message_id"].(string)+"/"+r["id"].(string)+".json"] = []byte(r["data"].(string))
		}
	}
	legacy := parse(t, in)
	if !reflect.DeepEqual(db, legacy) {
		t.Errorf("sqlite:\n%+v\nlegacy-json:\n%+v", db, legacy)
	}
}
