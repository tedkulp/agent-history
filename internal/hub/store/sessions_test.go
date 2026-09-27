package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	sessUUID  = "3d34bfcc-90e7-4fd0-900f-86c047b22433"
	mainKey   = "-Users-ted-src-app/" + sessUUID + ".jsonl"
	childKey  = "-Users-ted-src-app/" + sessUUID + "/subagents/agent-a1.jsonl"
	resultKey = "-Users-ted-src-app/" + sessUUID + "/tool-results/x.txt"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openParsing(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir(), parser.NewRegistry(claudecode.New()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &fakeClock{t: time.UnixMilli(1_800_000_000_000)}
	s.SetClock(c.now)
	if err := s.UpsertMachine(context.Background(), "m1", protocol.MachineInfo{Hostname: "laptop", HomeDir: "/Users/ted"}); err != nil {
		t.Fatal(err)
	}
	return s, c
}

// appendTo appends data at the record's current end.
func appendTo(t *testing.T, s *Store, source, key string, data []byte) {
	t.Helper()
	prev, err := s.CurrentContent(context.Background(), "m1", source, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Append(context.Background(), AppendRequest{
		MachineID: "m1", Source: source, RecordKey: key,
		Offset: int64(len(prev)), PrefixSha256: hexSum(prev),
		Data: data, Compressed: zstdBytes(t, data),
	})
	if err != nil {
		t.Fatal(err)
	}
}

type queueRow struct {
	priority, enqueuedAt, notBefore int64
}

func queueOf(t *testing.T, s *Store, sessionID int64) (queueRow, bool) {
	t.Helper()
	var q queueRow
	err := s.read.QueryRow(`SELECT priority, enqueued_at, not_before FROM parse_queue WHERE session_id = ?`, sessionID).
		Scan(&q.priority, &q.enqueuedAt, &q.notBefore)
	if err == sql.ErrNoRows {
		return q, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return q, true
}

func sessionIDOf(t *testing.T, s *Store, native string) int64 {
	t.Helper()
	var id int64
	if err := s.read.QueryRow(`SELECT id FROM sessions WHERE native_id = ?`, native).Scan(&id); err != nil {
		t.Fatalf("session %q: %v", native, err)
	}
	return id
}

const line1 = `{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"hello"}}` + "\n"
const line2 = `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_1","model":"claude-opus-5-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"

func TestIngestAttachesRecordsAndEnqueues(t *testing.T) {
	s, c := openParsing(t)
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	appendTo(t, s, protocol.SourceClaudeCode, resultKey, []byte("out"))
	appendTo(t, s, "some-future-source", "a/b", []byte("x"))

	id := sessionIDOf(t, s, sessUUID)
	rows, err := s.read.Query(`SELECT record_key, session_id, role, layout, layout_rank FROM raw_records ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rec struct {
		key     string
		session sql.NullInt64
		role    sql.NullString
		layout  sql.NullString
		rank    sql.NullInt64
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.key, &r.session, &r.role, &r.layout, &r.rank); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 ||
		got[0].session.Int64 != id || got[0].role.String != "main" || got[0].layout.String != "jsonl" || got[0].rank.Int64 != 1 ||
		got[1].session.Int64 != id || got[1].role.String != "attachment" ||
		got[2].session.Valid || got[2].role.Valid {
		t.Fatalf("records = %+v", got)
	}

	q, ok := queueOf(t, s, id)
	if !ok || q.priority != 0 || q.notBefore != c.t.UnixMilli() {
		t.Fatalf("queue = %+v, %v", q, ok)
	}
	select {
	case <-s.Wake():
	default:
		t.Fatal("ingest did not signal the worker")
	}
}

// parseNext runs one job the way the worker does.
func parseNext(t *testing.T, s *Store) bool {
	t.Helper()
	ctx := context.Background()
	job, ok, _, err := s.NextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return false
	}
	in, ok, err := s.LoadParseInput(ctx, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		if err := s.DropJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		return true
	}
	p := claudecode.New()
	res, err := p.Parse(in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveParse(ctx, job, p.Version(), res); err != nil {
		t.Fatal(err)
	}
	return true
}

func TestLiveEnqueueCoalescesToOneParsePer10s(t *testing.T) {
	s, c := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	id := sessionIDOf(t, s, sessUUID)
	if !parseNext(t, s) {
		t.Fatal("first append was not due immediately")
	}
	parsedAt := c.t.UnixMilli()
	if _, ok := queueOf(t, s, id); ok {
		t.Fatal("queue row left after parse")
	}

	// A burst of appends right after the parse waits for the 10 s interval.
	for i := 0; i < 5; i++ {
		c.advance(time.Second)
		appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line2))
	}
	q, _ := queueOf(t, s, id)
	if q.notBefore != parsedAt+10_000 {
		t.Fatalf("not_before = %d, want %d", q.notBefore, parsedAt+10_000)
	}
	_, ok, next, err := s.NextJob(ctx)
	if err != nil || ok || next != parsedAt+10_000 {
		t.Fatalf("NextJob before the interval: ok=%v next=%d err=%v", ok, next, err)
	}
	c.advance(5 * time.Second)
	if !parseNext(t, s) {
		t.Fatal("job not due after 10 s")
	}
	if parseNext(t, s) {
		t.Fatal("the burst caused a second parse")
	}
}

func TestDataArrivingDuringParseKeepsTheJob(t *testing.T) {
	s, c := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	id := sessionIDOf(t, s, sessUUID)

	job, ok, _, err := s.NextJob(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	in, _, _ := s.LoadParseInput(ctx, id)
	// New data lands in the same millisecond, before the parse is saved.
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line2))
	res, _ := claudecode.New().Parse(in.Input)
	if err := s.SaveParse(ctx, job, 1, res); err != nil {
		t.Fatal(err)
	}
	q, ok := queueOf(t, s, id)
	if !ok || q.notBefore != c.t.UnixMilli()+10_000 {
		t.Fatalf("queue = %+v, %v", q, ok)
	}
}

func TestSaveParseBuildsFeedAndTranscript(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1+line2))
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"child task"}}` + "\n"
	appendTo(t, s, protocol.SourceClaudeCode, childKey, []byte(child))
	for parseNext(t, s) {
	}

	feed, err := s.Feed(ctx, FeedFilter{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 1 {
		t.Fatalf("feed = %+v", feed)
	}
	r := feed[0]
	if r.Title != "hello" || r.FirstPrompt != "hello" || r.ProjectCwd != "/Users/ted/src/app" || r.Machine != "laptop" || r.Source != "claude-code" {
		t.Errorf("feed row = %+v", r)
	}

	h, msgs, ok, err := s.Transcript(ctx, r.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if h.Model != "claude-opus-5-5" || len(msgs) != 2 || msgs[0].ID != "u1" || msgs[1].ID != "a1" ||
		msgs[1].Parts[0].ID != "a1.0" || msgs[1].Parts[0].Payload != `{"text":"hi"}` {
		t.Fatalf("header = %+v, messages = %+v", h, msgs)
	}
	var usage string
	s.read.QueryRow(`SELECT usage_json FROM sessions WHERE id = ?`, r.ID).Scan(&usage)
	if usage != `{"input":1,"output":2,"cache_read":0,"cache_write":0,"reasoning":0}` {
		t.Errorf("usage_json = %s", usage)
	}

	// The Child Session links to its parent and stays out of the feed.
	var parent sql.NullInt64
	s.read.QueryRow(`SELECT parent_session_id FROM sessions WHERE native_id = ?`, sessUUID+"/agent-a1").Scan(&parent)
	if parent.Int64 != r.ID {
		t.Errorf("child parent = %v, want %d", parent, r.ID)
	}

	// A stub Session is a 404.
	if _, _, ok, _ := s.Transcript(ctx, 999); ok {
		t.Error("unknown Session found")
	}
}

func TestParentStubIsHiddenUntilParsed(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	child := `{"type":"user","uuid":"c1","parentUuid":null,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/tmp","message":{"role":"user","content":"child"}}` + "\n"
	appendTo(t, s, protocol.SourceClaudeCode, childKey, []byte(child))
	for parseNext(t, s) {
	}
	stub := sessionIDOf(t, s, sessUUID)
	if feed, _ := s.Feed(ctx, FeedFilter{}, 50); len(feed) != 0 {
		t.Errorf("feed = %+v", feed)
	}
	if _, _, ok, _ := s.Transcript(ctx, stub); ok {
		t.Error("stub Session has a Transcript page")
	}
}

func TestReparseKeepsIds(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1+line2))
	parseNext(t, s)
	id := sessionIDOf(t, s, sessUUID)
	_, before, _, _ := s.Transcript(ctx, id)

	if _, err := s.write.Exec(`INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before) VALUES (?, 1, 0, 0)`, id); err != nil {
		t.Fatal(err)
	}
	parseNext(t, s)
	_, after, _, _ := s.Transcript(ctx, id)
	if len(before) == 0 || len(before) != len(after) {
		t.Fatalf("before %d, after %d messages", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Parts[0].ID != after[i].Parts[0].ID {
			t.Errorf("message %d: %s/%s became %s/%s", i, before[i].ID, before[i].Parts[0].ID, after[i].ID, after[i].Parts[0].ID)
		}
	}
}

func TestSaveFailureKeepsTranscript(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	parseNext(t, s)
	id := sessionIDOf(t, s, sessUUID)
	if _, err := s.write.Exec(`INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before) VALUES (?, 1, 0, 0)`, id); err != nil {
		t.Fatal(err)
	}
	job, _, _, _ := s.NextJob(ctx)
	if err := s.SaveFailure(ctx, job, 1, "boom"); err != nil {
		t.Fatal(err)
	}
	var status, perr string
	s.read.QueryRow(`SELECT parse_status, parse_error FROM sessions WHERE id = ?`, id).Scan(&status, &perr)
	if status != "failed" || perr != "boom" {
		t.Errorf("status = %s, error = %s", status, perr)
	}
	if _, msgs, ok, _ := s.Transcript(ctx, id); !ok || len(msgs) != 1 {
		t.Error("previous Transcript lost")
	}
	if _, ok := queueOf(t, s, id); ok {
		t.Error("queue row kept")
	}
}

func TestNextJobOrder(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	mk := func(native string, last int64) int64 {
		id, err := sessionFor(ctx, s.write, "m1", "claude-code", native)
		if err != nil {
			t.Fatal(err)
		}
		s.write.Exec(`UPDATE sessions SET last_activity_at = ? WHERE id = ?`, last, id)
		return id
	}
	old, recent, live := mk("old", 100), mk("recent", 200), mk("live", 50)
	s.write.Exec(`INSERT INTO parse_queue VALUES (?, 1, 0, 0), (?, 1, 0, 0), (?, 0, 0, 5)`, old, recent, live)

	var order []int64
	for {
		job, ok, _, err := s.NextJob(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		order = append(order, job.SessionID)
		s.DropJob(ctx, job)
	}
	if len(order) != 3 || order[0] != live || order[1] != recent || order[2] != old {
		t.Errorf("order = %v, want live %d, recent %d, old %d", order, live, recent, old)
	}
}

func TestProjectCwd(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"/Users/ted":                "",
		"/Users/ted/":               "",
		"/tmp":                      "",
		"/private/tmp/x":            "",
		"/var/tmp":                  "",
		"/var/folders/ab/T/x":       "",
		"/private/var/folders/ab/T": "",
		"/tmpfoo":                   "/tmpfoo",
		"/Users/ted/src/app/":       "/Users/ted/src/app",
	}
	for cwd, want := range cases {
		if got := ProjectCwd(cwd, "/Users/ted/"); got != want {
			t.Errorf("ProjectCwd(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestHomeDirChangeReassignsProjects(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	homeLine := `{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/home/ted","message":{"role":"user","content":"hi"}}` + "\n"
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(homeLine))
	parseNext(t, s)
	project := func() sql.NullString {
		var p sql.NullString
		s.read.QueryRow(`SELECT project_cwd FROM sessions WHERE native_id = ?`, sessUUID).Scan(&p)
		return p
	}
	if p := project(); p.String != "/home/ted" {
		t.Fatalf("with home /Users/ted: project = %v", p)
	}
	if err := s.UpsertMachine(ctx, "m1", protocol.MachineInfo{Hostname: "laptop", HomeDir: "/home/ted"}); err != nil {
		t.Fatal(err)
	}
	if p := project(); p.Valid {
		t.Errorf("after home_dir change: project = %v, want No project", p)
	}
	if job, ok, _, _ := s.NextJob(ctx); ok {
		t.Errorf("home_dir change queued a re-parse: %+v", job)
	}
}

func TestReplaceEnqueuesAndParsesNewVersion(t *testing.T) {
	s, c := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"old prompt"}}`+"\n"))
	id := sessionIDOf(t, s, sessUUID)
	c.advance(time.Minute)
	for parseNext(t, s) {
	}
	if _, ok := queueOf(t, s, id); ok {
		t.Fatal("queue row left after parse")
	}

	neu := []byte(`{"type":"user","uuid":"u2","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"new prompt"}}` + "\n")
	if _, err := s.Replace(ctx, ReplaceRequest{MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: mainKey, Data: neu, Compressed: zstdBytes(t, neu)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := queueOf(t, s, id); !ok {
		t.Fatal("replace did not enqueue the Session")
	}
	c.advance(time.Minute)
	for parseNext(t, s) {
	}
	var prompt string
	if err := s.read.QueryRowContext(ctx, `SELECT first_prompt FROM sessions WHERE id = ?`, id).Scan(&prompt); err != nil {
		t.Fatal(err)
	}
	if prompt != "new prompt" {
		t.Fatalf("first_prompt = %q, want the replaced content", prompt)
	}

	// An identical replace changes nothing and enqueues nothing.
	if _, err := s.Replace(ctx, ReplaceRequest{MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: mainKey, Data: neu, Compressed: zstdBytes(t, neu)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := queueOf(t, s, id); ok {
		t.Fatal("identical replace enqueued a parse")
	}
}

func TestSaveParseSplitsToolOutputAndStoresBlobs(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	str := func(n int) *string { v := strings.Repeat("é", n/2); return &v } // n bytes
	call := func(id string, out *string) parser.Part {
		return parser.Part{ID: id, Kind: parser.KindToolCall, Payload: parser.ToolCallPayload{
			CallID: id, Name: "Bash", Input: json.RawMessage(`{}`), Status: "ok", Output: out, ChildSessions: []string{},
		}}
	}
	imgPayload, img := parser.NewImage("image/png", []byte("png bytes"))
	res := parser.Result{
		Messages: []parser.Message{{ID: "a1", Role: parser.MessageAssistant, Parts: []parser.Part{
			call("a1.0", str(4096)),  // at 4 KB: inline, no preview
			call("a1.1", str(4098)),  // over 4 KB: inline with preview
			call("a1.2", str(16386)), // over 16 KB: split out
			call("a1.3", nil),        // pending
			{ID: "a1.4", Kind: parser.KindImage, Payload: imgPayload},
		}}},
		Images: []parser.Image{img, img},
	}
	save := func() int64 {
		t.Helper()
		job, ok, _, err := s.NextJob(ctx)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := s.SaveParse(ctx, job, 1, res); err != nil {
			t.Fatal(err)
		}
		return job.SessionID
	}
	id := save()

	payload := func(part string) parser.ToolCallPayload {
		t.Helper()
		var js string
		if err := s.read.QueryRow(`SELECT payload_json FROM parts WHERE session_id = ? AND id = ?`, id, part).Scan(&js); err != nil {
			t.Fatal(err)
		}
		var tc parser.ToolCallPayload
		if err := json.Unmarshal([]byte(js), &tc); err != nil {
			t.Fatal(err)
		}
		return tc
	}
	if p := payload("a1.0"); p.Output == nil || p.OutputSize != 4096 || p.OutputPreview != "" {
		t.Errorf("4 KB output: size %d, preview %q, inline %v", p.OutputSize, p.OutputPreview, p.Output != nil)
	}
	if p := payload("a1.1"); p.Output == nil || p.OutputSize != 4098 || p.OutputPreview != strings.Repeat("é", 200) {
		t.Errorf("over 4 KB: size %d, preview %q, inline %v", p.OutputSize, p.OutputPreview, p.Output != nil)
	}
	if p := payload("a1.2"); p.Output != nil || p.OutputSize != 16386 || p.OutputPreview == "" {
		t.Errorf("over 16 KB: size %d, inline %v", p.OutputSize, p.Output != nil)
	}
	if p := payload("a1.3"); p.Output != nil || p.OutputSize != 0 || p.Status != "ok" {
		t.Errorf("no output: %+v", p)
	}

	for part, want := range map[string]string{"a1.1": *str(4098), "a1.2": *str(16386), "a1.3": ""} {
		got, ok, err := s.ToolOutput(ctx, id, part)
		if err != nil || !ok || got != want {
			t.Errorf("ToolOutput(%s) = %d bytes, %v, %v; want %d bytes", part, len(got), ok, err, len(want))
		}
	}
	if _, ok, err := s.ToolOutput(ctx, id, "a1.4"); ok || err != nil {
		t.Errorf("ToolOutput of an image Part = %v, %v", ok, err)
	}

	mime, b, ok, err := s.Blob(ctx, img.SHA256)
	if err != nil || !ok || mime != "image/png" || string(b) != "png bytes" {
		t.Errorf("Blob = %q %q %v %v", mime, b, ok, err)
	}
	if _, _, ok, _ := s.Blob(ctx, "00"); ok {
		t.Error("unknown blob found")
	}

	// A re-parse replaces tool_outputs and keeps blobs deduplicated.
	if _, err := s.write.Exec(`INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before) VALUES (?, 1, 0, 0)`, id); err != nil {
		t.Fatal(err)
	}
	save()
	var outputs, blobs int
	s.read.QueryRow(`SELECT count(*) FROM tool_outputs`).Scan(&outputs)
	s.read.QueryRow(`SELECT count(*) FROM blobs`).Scan(&blobs)
	if outputs != 1 || blobs != 1 {
		t.Errorf("tool_outputs = %d, blobs = %d; want 1, 1", outputs, blobs)
	}
}

func TestSaveParseRebuildsParseWarnings(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	lines := line1 + `{"type":"brand_new","x":1}` + "\n" + `{"type":"brand_new","x":2}` + "\n" + "{not json\n" + line2
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(strings.Replace(lines, `"cwd"`, `"version":"2.1.300","cwd"`, 1)))
	parseNext(t, s)
	id := sessionIDOf(t, s, sessUUID)

	h, _, ok, err := s.Transcript(ctx, id)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := []ParseWarning{
		{Kind: "unknown_type", SourceType: "brand_new", Count: 2, FirstExcerpt: `{"type":"brand_new","x":1}`},
		{Kind: "bad_line", SourceType: "", Count: 1, FirstExcerpt: "{not json"},
	}
	if h.SourceVersion != "2.1.300" || !reflect.DeepEqual(h.Warnings, want) {
		t.Fatalf("version = %q, warnings = %+v", h.SourceVersion, h.Warnings)
	}

	// A re-parse replaces the rows rather than adding to them.
	if _, err := s.write.Exec(`INSERT INTO parse_warnings (session_id, kind, source_type, count) VALUES (?, 'orphan', 'stale', 1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.Exec(`INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before) VALUES (?, 1, 0, 0)`, id); err != nil {
		t.Fatal(err)
	}
	parseNext(t, s)
	h, _, _, _ = s.Transcript(ctx, id)
	if !reflect.DeepEqual(h.Warnings, want) {
		t.Errorf("after re-parse: %+v", h.Warnings)
	}
}

func TestParentFirstParseReenqueuesOrphanedChildren(t *testing.T) {
	s, c := openParsing(t)
	// The orphan's first line names a parent line that isn't in its file.
	orphan := `{"type":"user","uuid":"c1","parentUuid":"gone","isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/tmp","message":{"role":"user","content":"child"}}` + "\n"
	clean := `{"type":"user","uuid":"c2","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/tmp","message":{"role":"user","content":"child"}}` + "\n"
	otherKey := "-Users-ted-src-app/" + sessUUID + "/subagents/agent-b2.jsonl"
	appendTo(t, s, protocol.SourceClaudeCode, childKey, []byte(orphan))
	appendTo(t, s, protocol.SourceClaudeCode, otherKey, []byte(clean))
	for parseNext(t, s) {
	}
	orphanID, cleanID := sessionIDOf(t, s, sessUUID+"/agent-a1"), sessionIDOf(t, s, sessUUID+"/agent-b2")
	var parent int64
	if err := s.read.QueryRow(`SELECT parent_session_id FROM sessions WHERE id = ?`, orphanID).Scan(&parent); err != nil || parent != sessionIDOf(t, s, sessUUID) {
		t.Fatalf("parent = %d, %v", parent, err)
	}

	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	parseNext(t, s)
	if q, ok := queueOf(t, s, orphanID); !ok || q.priority != 1 {
		t.Errorf("orphaned child queue = %+v, %v", q, ok)
	}
	if _, ok := queueOf(t, s, cleanID); ok {
		t.Error("child without an orphan warning re-enqueued")
	}

	// Only the parent's first parse re-enqueues.
	for parseNext(t, s) {
	}
	c.advance(parseInterval)
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line2))
	c.advance(parseInterval)
	for parseNext(t, s) {
	}
	var parsedAt int64
	s.read.QueryRow(`SELECT parsed_at FROM sessions WHERE id = ?`, parent).Scan(&parsedAt)
	if parsedAt != c.t.UnixMilli() {
		t.Fatalf("parent not re-parsed")
	}
	if _, ok := queueOf(t, s, orphanID); ok {
		t.Error("a later parse of the parent re-enqueued the child")
	}
}
