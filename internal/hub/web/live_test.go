package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/live"
	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

// liveSite is a Hub serving one growing Claude Code Session, with a clock
// the test moves past the 10 s live-parse interval.
type liveSite struct {
	t      *testing.T
	srv    *httptest.Server
	store  *store.Store
	worker *worker.Worker
	live   *live.Broadcaster
	skew   atomic.Int64 // added to the store's clock, in ms
	body   []byte       // the Source file so far
}

const liveKey = "-Users-ted-src-app/" + sess + ".jsonl"

func newLiveSite(t *testing.T, first string) *liveSite {
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
	ls := &liveSite{t: t, store: s, live: live.New()}
	s.SetClock(func() time.Time { return time.Now().Add(time.Duration(ls.skew.Load()) * time.Millisecond) })
	ls.worker = worker.New(s, reg, nil).WithLive(ls.live)
	ls.srv = httptest.NewServer(New(s, ls.live, nil))
	t.Cleanup(ls.srv.Close)
	ls.append(first)
	ls.parse()
	return ls
}

// append adds lines to the Source file and moves the clock past the live
// interval, so the next parse picks them up.
func (ls *liveSite) append(lines string) {
	ls.t.Helper()
	enc, _ := zstd.NewWriter(nil)
	prefix := sha256.Sum256(ls.body)
	data := []byte(lines)
	if _, err := ls.store.Append(context.Background(), store.AppendRequest{
		MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: liveKey, Offset: int64(len(ls.body)),
		PrefixSha256: hex.EncodeToString(prefix[:]), Data: data, Compressed: enc.EncodeAll(data, nil),
	}); err != nil {
		ls.t.Fatal(err)
	}
	ls.body = append(ls.body, data...)
	ls.skew.Add((11 * time.Second).Milliseconds())
}

// parse runs every due parse job.
func (ls *liveSite) parse() {
	ls.t.Helper()
	for {
		worked, _, err := ls.worker.RunOnce(context.Background())
		if err != nil {
			ls.t.Fatal(err)
		}
		if !worked {
			return
		}
	}
}

var (
	dataAfterRe = regexp.MustCompile(`data-after="([^"]*)" data-count="(\d+)"`)
)

// state is the page's (or update's) last Message id and Message count.
func state(t *testing.T, body string) (after, count string) {
	t.Helper()
	m := dataAfterRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no live state in:\n%s", body)
	}
	return m[1], m[2]
}

func (ls *liveSite) tail(after, count string) (int, string) {
	ls.t.Helper()
	return get(ls.t, ls.srv.URL+"/sessions/1/messages?after="+url.QueryEscape(after)+"&count="+count)
}

func line(uuid, parent, role, content string) string {
	p := "null"
	if parent != "" {
		p = `"` + parent + `"`
	}
	if role == "user" {
		return `{"type":"user","uuid":"` + uuid + `","parentUuid":` + p + `,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/Users/ted/src/app","gitBranch":"main","message":{"role":"user","content":` + content + `}}` + "\n"
	}
	return `{"type":"assistant","uuid":"` + uuid + `","parentUuid":` + p + `,"timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_` + uuid + `","model":"claude-opus-5-5","content":` + content + `}}` + "\n"
}

func TestTranscriptTailAppendsAndFinishesToolCalls(t *testing.T) {
	ls := newLiveSite(t,
		line("u1", "", "user", `"first prompt"`)+
			line("a1", "u1", "assistant", `[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]`))

	code, page := get(t, ls.srv.URL+"/sessions/1")
	if code != 200 {
		t.Fatalf("page status %d", code)
	}
	after, count := state(t, page)
	if after != "a1" || count != "2" {
		t.Fatalf("page state = %q, %q", after, count)
	}
	for _, want := range []string{`data-events="/sessions/1/events"`, `data-messages="/sessions/1/messages"`, "⋯"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// Nothing new: the last Message comes back alone.
	code, upd := ls.tail(after, count)
	if code != 200 || !strings.Contains(upd, `id="m-a1"`) || strings.Contains(upd, `id="m-u1"`) {
		t.Fatalf("idle tail (%d):\n%s", code, upd)
	}

	// The tool call finishes and a new prompt arrives.
	ls.append(line("r1", "a1", "user", `[{"type":"tool_result","tool_use_id":"t1","content":"file.txt"}]`) +
		line("a2", "r1", "assistant", `[{"type":"text","text":"Listed."}]`) +
		line("u2", "a2", "user", `"second prompt"`))
	ls.parse()

	code, upd = ls.tail(after, count)
	if code != 200 {
		t.Fatalf("tail status %d:\n%s", code, upd)
	}
	for _, want := range []string{`id="m-a1"`, "file.txt", `id="m-u2"`, "Listed.", `data-m="u2"`, "second prompt", "⎇ main", "claude-opus-5-5", `class="outline-children"`} {
		if !strings.Contains(upd, want) {
			t.Errorf("update lacks %q", want)
		}
	}
	if strings.Contains(upd, `id="m-u1"`) || strings.Contains(upd, `data-m="u1"`) {
		t.Error("update repeats a Message before the page's last one")
	}
	if strings.Contains(upd, "⋯") {
		t.Error("the finished tool call is still pending")
	}
	if a, c := state(t, upd); a != "u2" || c != "4" {
		// r1 is a tool result folded into a1, so the Transcript holds u1, a1, a2, u2.
		t.Errorf("update state = %q, %q", a, c)
	}
}

func TestTranscriptTailRefusesAReshuffle(t *testing.T) {
	ls := newLiveSite(t,
		line("u1", "", "user", `"first prompt"`)+
			line("a1", "u1", "assistant", `[{"type":"text","text":"one"}]`)+
			line("u2", "a1", "user", `"second prompt"`)+
			line("a2", "u2", "assistant", `[{"type":"text","text":"two"}]`))
	_, page := get(t, ls.srv.URL+"/sessions/1")
	after, count := state(t, page)

	// A rewind: a new branch from u1 becomes the latest leaf.
	ls.append(line("u3", "a1", "user", `"rewound prompt"`) + line("a3", "u3", "assistant", `[{"type":"text","text":"three"}]`))
	ls.parse()
	if code, body := ls.tail(after, count); code != http.StatusConflict {
		t.Fatalf("tail after a rewind: %d\n%s", code, body)
	}

	for _, q := range []string{"?after=a1&count=x", "?after=a1&count=-1", "?after=a1"} {
		if code, _ := get(t, ls.srv.URL+"/sessions/1/messages"+q); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, code)
		}
	}
	for _, p := range []string{"/sessions/999/messages?after=&count=0", "/sessions/x/messages?after=&count=0"} {
		if code, _ := get(t, ls.srv.URL+p); code != http.StatusNotFound {
			t.Errorf("%s: status %d", p, code)
		}
	}
}

// events opens a Session's event stream and returns its status and a reader
// of its lines.
func events(t *testing.T, srvURL string, id string) (*http.Response, *bufio.Reader) {
	t.Helper()
	return stream(t, srvURL+"/sessions/"+id+"/events")
}

// stream opens an event stream and returns its status and a reader of its
// lines.
func stream(t *testing.T, u string) (*http.Response, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body)
}

// nextEvent reads stream lines until one starts with prefix and returns it
// without its newline, failing after a timeout.
func nextEvent(t *testing.T, r *bufio.Reader, prefix string) string {
	t.Helper()
	type read struct {
		line string
		err  error
	}
	got := make(chan read, 1)
	go func() {
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				got <- read{err: err}
				return
			}
			if strings.HasPrefix(l, prefix) {
				got <- read{line: strings.TrimSuffix(l, "\n")}
				return
			}
		}
	}()
	select {
	case g := <-got:
		if g.err != nil {
			t.Fatalf("stream ended before %q: %v", prefix, g.err)
		}
		return g.line
	case <-time.After(5 * time.Second):
		t.Fatalf("no %q on the stream", prefix)
	}
	return ""
}

func TestEventsStreamLiveParsesWithoutReadingTheStore(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	defer func() { heartbeatEvery = old }()

	ls := newLiveSite(t, line("u1", "", "user", `"first prompt"`))
	resp, r := events(t, ls.srv.URL, "1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "X-Accel-Buffering": "no", "Cache-Control": "no-cache"} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	nextEvent(t, r, ": heartbeat")

	// A live parse reaches the open stream.
	ls.append(line("a1", "u1", "assistant", `[{"type":"text","text":"hi"}]`))
	ls.parse()
	nextEvent(t, r, "event: changed")

	// With the store closed, the idle stream keeps beating and still carries
	// events: it reads nothing until the page fetches.
	ls.store.Close()
	nextEvent(t, r, ": heartbeat")
	ls.live.Publish(1)
	nextEvent(t, r, "event: changed")

	// Shutdown ends the stream.
	ls.live.Close()
	done := make(chan struct{})
	go func() {
		for {
			if _, err := r.ReadString('\n'); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived Close")
	}
}

func TestEventsRejectsStubsAndUnknownSessions(t *testing.T) {
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/tmp","message":{"role":"user","content":"child task"}}` + "\n"
	srv := newSite(t, map[string]string{"-Users-ted-src-app/" + sess + "/subagents/agent-a.jsonl": child})
	// The child is Session 1; its parse creates the parent stub, 2.
	for id, want := range map[string]int{"2": 404, "999": 404, "x": 404} {
		if code, _ := get(t, srv.URL+"/sessions/"+id+"/events"); code != want {
			t.Errorf("events for %s: status %d, want %d", id, code, want)
		}
	}
	resp, _ := events(t, srv.URL, "1")
	if resp.StatusCode != 200 {
		t.Errorf("events for a parsed Child Session: status %d", resp.StatusCode)
	}
}
