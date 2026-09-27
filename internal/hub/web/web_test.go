package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	days := groupByDay(rows, now, "")
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

type record struct{ machine, key, body string }

func newSite(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	var recs []record
	for key, body := range files {
		recs = append(recs, record{"m1", key, body})
	}
	return newSiteRecords(t, recs)
}

// newSiteRecords serves a Hub holding the given Claude Code records, parsed.
// Machine m1 is "laptop" with home /Users/ted; m2 is "desk" with home /home/ted.
func newSiteRecords(t *testing.T, recs []record) *httptest.Server {
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
	for _, r := range recs {
		if _, err := s.Append(ctx, store.AppendRequest{
			MachineID: r.machine, Source: protocol.SourceClaudeCode, RecordKey: r.key,
			PrefixSha256: hex.EncodeToString(empty[:]), Data: []byte(r.body), Compressed: enc.EncodeAll([]byte(r.body), nil),
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
	if !strings.Contains(body, `id="m-a2"`) || !strings.Contains(body, "⚙ 1 tool call · Bash") {
		t.Error("a tool-call-only Message isn't shown as a cluster")
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

// sessionLine is a one-Message Claude Code Session started in cwd at minute i
// of the day.
func sessionLine(cwd, prompt string, i int) string {
	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04:05.000Z")
	return `{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"` + ts + `","cwd":"` + cwd + `","message":{"role":"user","content":"` + prompt + `"}}` + "\n"
}

func uuid(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }

func TestFeedChipsFilterAndLiveInTheURL(t *testing.T) {
	srv := newSiteRecords(t, []record{
		{"m1", "-Users-ted-src-app/" + uuid(1) + ".jsonl", sessionLine("/Users/ted/src/app", "app one", 1)},
		{"m1", "-Users-ted-src-app/" + uuid(2) + ".jsonl", sessionLine("/Users/ted/src/app", "app two", 2)},
		{"m1", "-Users-ted/" + uuid(3) + ".jsonl", sessionLine("/Users/ted", "home one", 3)},
		{"m2", "-home-ted-src-app/" + uuid(4) + ".jsonl", sessionLine("/home/ted/src/app", "desk one", 4)},
	})

	_, body := get(t, srv.URL+"/")
	for _, want := range []string{`>laptop`, `>desk`, `href="/?machine=m1"`, `href="/?source=claude-code"`, "app one", "desk one"} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(body, ">Project<") {
		t.Error("Project chips shown with no Machine picked")
	}

	_, body = get(t, srv.URL+"/?machine=m1")
	for _, want := range []string{
		`class="chip on" href="/" title="m1"`, // toggles off
		">Project<", `href="/?machine=m1&amp;project=%2FUsers%2Fted%2Fsrc%2Fapp"`, `title="/Users/ted/src/app"`,
		`href="/?machine=m1&amp;project=-"`, "No project",
		"app one", "home one",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("machine=m1 lacks %q", want)
		}
	}
	if strings.Contains(body, "desk one") {
		t.Error("machine=m1 lists a Session of m2")
	}
	if !strings.Contains(body, `<span class="n">2</span>`) || !strings.Contains(body, `<span class="n">1</span>`) {
		t.Error("Project chips lack Session counts")
	}

	_, body = get(t, srv.URL+"/?machine=m1&project=-")
	if !strings.Contains(body, "home one") || strings.Contains(body, "app one") {
		t.Error("No project chip does not filter")
	}
	_, body = get(t, srv.URL+"/?machine=m1&project=%2FUsers%2Fted%2Fsrc%2Fapp&source=claude-code")
	if !strings.Contains(body, "app two") || strings.Contains(body, "home one") {
		t.Error("combined chips do not filter")
	}
	// Toggling the Machine off drops its Project too; the Source stays.
	if !strings.Contains(body, `class="chip on" href="/?source=claude-code"`) {
		t.Error("Machine chip does not toggle off to the remaining chips")
	}
	_, body = get(t, srv.URL+"/?source=codex")
	if !strings.Contains(body, "No Sessions match.") {
		t.Error("an empty filtered feed lacks its note")
	}
}

func TestFeedLoadMore(t *testing.T) {
	var recs []record
	const n = 2*feedPageSize + 7
	for i := 1; i <= n; i++ {
		recs = append(recs, record{"m1", "-Users-ted-src-app/" + uuid(i) + ".jsonl", sessionLine("/Users/ted/src/app", fmt.Sprintf("prompt-%03d", i), i/4)})
	}
	srv := newSiteRecords(t, recs)

	seen := map[string]bool{}
	var order []string
	collect := func(body string) {
		for _, part := range strings.Split(body, `class="p">prompt-`)[1:] {
			p := part[:3]
			if seen[p] {
				t.Errorf("prompt-%s listed twice", p)
			}
			seen[p] = true
			order = append(order, p)
		}
	}
	nextURL := func(body string) string {
		i := strings.Index(body, `hx-get="`)
		if i < 0 {
			return ""
		}
		u := body[i+len(`hx-get="`):]
		return strings.ReplaceAll(u[:strings.Index(u, `"`)], "&amp;", "&")
	}

	_, body := get(t, srv.URL+"/?machine=m1")
	collect(body)
	pages := 1
	for u := nextURL(body); u != ""; u = nextURL(body) {
		if !strings.Contains(u, "machine=m1") {
			t.Errorf("Load more drops the chips: %s", u)
		}
		req, _ := http.NewRequest("GET", srv.URL+u, nil)
		req.Header.Set("HX-Request", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
		if strings.Contains(body, "<html") || strings.Contains(body, `class="chips"`) {
			t.Fatal("htmx request got a full page")
		}
		if strings.Contains(body, `class="day"`) {
			t.Error("a page continuing the same day repeats its day header")
		}
		collect(body)
		pages++
	}
	if len(seen) != n || pages != 3 {
		t.Fatalf("saw %d Sessions over %d pages, want %d over 3", len(seen), pages, n)
	}
	if !sort.SliceIsSorted(order, func(i, j int) bool { return order[i] > order[j] }) {
		t.Errorf("feed out of order: %v", order)
	}
}

func TestTranscriptHeaderLinksToFilteredFeed(t *testing.T) {
	srv := newSiteRecords(t, []record{
		{"m1", "-Users-ted-src-app/" + uuid(1) + ".jsonl", sessionLine("/Users/ted/src/app", "app one", 1)},
		{"m1", "-Users-ted/" + uuid(2) + ".jsonl", sessionLine("/Users/ted", "home one", 2)},
	})
	_, body := get(t, srv.URL+"/?machine=m1&project=%2FUsers%2Fted%2Fsrc%2Fapp")
	i := strings.Index(body, `href="/sessions/`)
	link := body[i+len(`href="`):]
	_, body = get(t, srv.URL+link[:strings.Index(link, `"`)])
	for _, want := range []string{`href="/?machine=m1">laptop</a>`, `href="/?machine=m1&amp;project=%2FUsers%2Fted%2Fsrc%2Fapp" title="/Users/ted/src/app">app</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("header lacks %q", want)
		}
	}
	_, body = get(t, srv.URL+"/?machine=m1&project=-")
	i = strings.Index(body, `href="/sessions/`)
	link = body[i+len(`href="`):]
	_, body = get(t, srv.URL+link[:strings.Index(link, `"`)])
	if !strings.Contains(body, `href="/?machine=m1&amp;project=-"`) {
		t.Error("No project header link lacks project=-")
	}
}

func TestTranscriptToolCallsAndImages(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	big := strings.Repeat("x", 17000) + "<script>alert(2)</script>"
	mid := "<i>mid</i>" + strings.Repeat("y", 5000)
	lines := []map[string]any{
		{"type": "user", "uuid": "u1", "parentUuid": nil, "timestamp": "2026-09-01T10:00:00.000Z", "cwd": "/Users/ted/src/app",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "see"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
			}}},
	}
	parent := "u1"
	calls := []struct {
		id, name string
		input    map[string]any
		out      string
		err      bool
	}{
		{"t1", "Bash", map[string]any{"command": "echo"}, "<b>bold?</b>", false},
		{"t2", "Edit", map[string]any{"file_path": "/src/app/main.go", "old_string": "keep\nold line", "new_string": "keep\nnew line"}, "updated", false},
		{"t3", "Bash", map[string]any{"command": "false"}, "exit 1", true},
		{"t4", "Read", map[string]any{"file_path": "/big"}, big, false},
		{"t5", "Read", map[string]any{"file_path": "/mid"}, mid, false},
	}
	for i, c := range calls {
		a, r := fmt.Sprintf("a%d", i), fmt.Sprintf("r%d", i)
		lines = append(lines,
			map[string]any{"type": "assistant", "uuid": a, "parentUuid": parent, "timestamp": "2026-09-01T10:00:01.000Z",
				"message": map[string]any{"id": "msg_1", "model": "m", "content": []any{map[string]any{"type": "tool_use", "id": c.id, "name": c.name, "input": c.input}}}},
			map[string]any{"type": "user", "uuid": r, "parentUuid": a, "timestamp": "2026-09-01T10:00:02.000Z",
				"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": c.id, "content": c.out, "is_error": c.err}}}},
		)
		parent = r
	}
	var main strings.Builder
	for _, l := range lines {
		b, _ := json.Marshal(l)
		main.Write(b)
		main.WriteByte('\n')
	}
	srv := newSite(t, map[string]string{"-Users-ted-src-app/" + sess + ".jsonl": main.String()})

	_, feed := get(t, srv.URL+"/")
	i := strings.Index(feed, `href="/sessions/`)
	link := feed[i+len(`href="`):]
	link = link[:strings.Index(link, `"`)]
	code, page := get(t, srv.URL+link)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		"⚙ 5 tool calls · Bash ✗, Edit, Read",
		"&lt;b&gt;bold?&lt;/b&gt;", // output is text, not HTML
		`<div class="diff-file">/src/app/main.go</div>`,
		`<span class="del">- old line</span>`, `<span class="add">+ new line</span>`, `<span class="">  keep</span>`,
		"Output collapsed · 16.6 KB", "Output collapsed · 4.9 KB",
		"&lt;i&gt;mid&lt;/i&gt;", // the preview, escaped
		`class="st error"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
	if strings.Contains(page, "<b>bold?") || strings.Contains(page, "<i>mid") {
		t.Error("HTML in tool output was rendered")
	}
	if strings.Contains(page, strings.Repeat("x", 300)) {
		t.Error("output over 4 KB rendered inline")
	}

	// Each stub loads its full output, escaped.
	for _, c := range []struct{ part, want string }{{"a0.3", "&lt;script&gt;alert(2)"}, {"a0.4", "&lt;i&gt;mid&lt;/i&gt;"}} {
		url := link + "/parts/" + c.part + "/output"
		if !strings.Contains(page, `hx-get="`+url+`"`) {
			t.Errorf("no stub loading %s", url)
		}
		code, frag := get(t, srv.URL+url)
		if code != 200 || !strings.Contains(frag, c.want) || strings.Contains(frag, "<script>") || strings.Contains(frag, "<i>") {
			t.Errorf("%s: status %d, fragment %.200q", url, code, frag)
		}
	}
	for _, p := range []string{link + "/parts/nope/output", "/sessions/abc/parts/a0.3/output"} {
		if code, _ := get(t, srv.URL+p); code != 404 {
			t.Errorf("%s: status %d", p, code)
		}
	}

	// The pasted image shows inline and is served with an immutable cache header.
	j := strings.Index(page, `<img src="/blobs/`)
	if j < 0 {
		t.Fatal("no inline image")
	}
	src := page[j+len(`<img src="`):]
	src = src[:strings.Index(src, `"`)]
	resp, err := http.Get(srv.URL + src)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("blob: %d %v", resp.StatusCode, resp.Header)
	}
	for _, p := range []string{"/blobs/" + strings.Repeat("0", 64), "/blobs/xyz"} {
		if code, _ := get(t, srv.URL+p); code != 404 {
			t.Errorf("%s: status %d", p, code)
		}
	}
}
