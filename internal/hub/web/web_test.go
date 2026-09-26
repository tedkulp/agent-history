package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

func TestGroupByDay(t *testing.T) {
	loc := time.FixedZone("X", -4*3600)
	now := time.Date(2026, 9, 26, 9, 0, 0, 0, loc)
	at := func(d, h int) int64 { return time.Date(2026, 9, d, h, 30, 0, 0, loc).UnixMilli() }
	rows := []store.FeedRow{
		{ID: 1, LastActivityAt: at(26, 8), Title: "a", FirstPrompt: "line one\nline two"},
		{ID: 2, LastActivityAt: at(26, 0), NativeID: "n2"},
		{ID: 3, LastActivityAt: at(25, 23), ProjectCwd: "/Users/ted/src/app"},
		{ID: 4, LastActivityAt: at(1, 12)},
		{ID: 5, LastActivityAt: time.Date(2025, 12, 31, 12, 0, 0, 0, loc).UnixMilli()},
	}
	days := groupByDay(rows, now)
	var got []string
	for _, d := range days {
		got = append(got, d.Label)
	}
	want := []string{"Today", "Yesterday", "Tue, Sep 1", "Wed, Dec 31, 2025"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	today := days[0].Rows
	if len(today) != 2 || today[0].Time != "08:30" || today[0].Prompt != "line one line two" || today[1].Title != "n2" || today[0].Project != "No project" {
		t.Errorf("today = %+v", today)
	}
	if y := days[1].Rows[0]; y.Project != "app" || y.ProjectFull != "/Users/ted/src/app" {
		t.Errorf("project = %q / %q", y.Project, y.ProjectFull)
	}
}

const sess = "3d34bfcc-90e7-4fd0-900f-86c047b22433"

func newSite(t *testing.T, files map[string]string) *httptest.Server {
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
	enc, _ := zstd.NewWriter(nil)
	empty := sha256.Sum256(nil)
	for key, body := range files {
		if _, err := s.Append(ctx, store.AppendRequest{
			MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: key,
			PrefixSha256: hex.EncodeToString(empty[:]), Data: []byte(body), Compressed: enc.EncodeAll([]byte(body), nil),
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
	srv := httptest.NewServer(New(s, nil))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFeedAndTranscriptPages(t *testing.T) {
	main := `{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"please <script>alert(1)</script> help"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_1","model":"claude-opus-5-5","content":[{"type":"text","text":"Sure, **done**."}]}}
{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2026-09-01T10:00:02.000Z","message":{"id":"msg_2","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}
{"type":"custom-title","customTitle":"Fix the thing"}
`
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"child task"}}
`
	srv := newSite(t, map[string]string{
		"-Users-ted-src-app/" + sess + ".jsonl":                   main,
		"-Users-ted-src-app/" + sess + "/subagents/agent-a.jsonl": child,
	})

	code, body := get(t, srv.URL+"/")
	if code != 200 {
		t.Fatalf("feed status %d", code)
	}
	for _, want := range []string{"Fix the thing", "claude-code", "laptop", ">app<", `href="/sessions/`} {
		if !strings.Contains(body, want) {
			t.Errorf("feed lacks %q", want)
		}
	}
	if strings.Contains(body, "child task") {
		t.Error("Child Session listed in the feed")
	}
	i := strings.Index(body, `href="/sessions/`)
	link := body[i+len(`href="`):]
	link = link[:strings.Index(link, `"`)]

	code, body = get(t, srv.URL+link)
	if code != 200 {
		t.Fatalf("transcript status %d", code)
	}
	for _, want := range []string{`id="m-u1"`, `id="m-a1"`, "&lt;script&gt;", "<strong>done</strong>", "claude-opus-5-5", `class="time"`} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("HTML in a text Part was rendered")
	}
	if strings.Contains(body, `id="m-a2"`) {
		t.Error("a Message with nothing to show got a bubble")
	}
	if strings.Index(body, `id="m-u1"`) > strings.Index(body, `id="m-a1"`) {
		t.Error("Messages out of order")
	}

	for _, p := range []string{"/sessions/999", "/sessions/abc"} {
		if code, _ := get(t, srv.URL+p); code != 404 {
			t.Errorf("%s: status %d", p, code)
		}
	}
	for _, p := range []string{"/static/app.css", "/static/chroma.css", "/static/htmx.min.js"} {
		if code, _ := get(t, srv.URL+p); code != 200 {
			t.Errorf("%s: status %d", p, code)
		}
	}
}
