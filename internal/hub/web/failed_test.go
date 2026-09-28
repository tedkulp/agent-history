package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type panicky struct{ *claudecode.Parser }

func (panicky) Parse(parser.Input) (parser.Result, error) { panic("boom") }

func TestFailedSessionsAndReparseBanner(t *testing.T) {
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
	appendLine := func(native, line string) {
		t.Helper()
		key := "-Users-ted-src-app/" + native + ".jsonl"
		cur, err := s.CurrentContent(ctx, "m1", protocol.SourceClaudeCode, key)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(cur)
		if _, err := s.Append(ctx, store.AppendRequest{
			MachineID: "m1", Source: protocol.SourceClaudeCode, RecordKey: key, Offset: int64(len(cur)),
			PrefixSha256: hex.EncodeToString(sum[:]), Data: []byte(line), Compressed: enc.EncodeAll([]byte(line), nil),
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := worker.New(s, reg, nil)
	runAll := func() {
		t.Helper()
		for {
			worked, _, err := w.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				return
			}
		}
	}

	good, other, never := uuid(1), uuid(2), uuid(3)
	appendLine(good, sessionLine("/Users/ted/src/app", "good prompt", 1))
	appendLine(other, sessionLine("/Users/ted/src/app", "other prompt", 2))
	runAll()

	// The parser breaks: a new Session never parses, and an update to a
	// parsed one fails but keeps its Transcript.
	reg[protocol.SourceClaudeCode] = panicky{claudecode.New()}
	s.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	appendLine(never, sessionLine("/Users/ted/src/app", "lost prompt", 3))
	appendLine(good, `{"type":"user","uuid":"u9","parentUuid":"u1","timestamp":"2026-09-01T11:00:00.000Z","message":{"role":"user","content":"more"}}`+"\n")
	runAll()
	if _, err := s.Reparse(ctx, store.ReparseScope{Source: protocol.SourceClaudeCode}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(s, nil, nil))
	t.Cleanup(srv.Close)

	_, home := get(t, srv.URL+"/")
	if !strings.Contains(home, "Re-parsing 3 Sessions…") {
		t.Errorf("home has no re-parse banner:\n%s", home)
	}
	if n := strings.Count(home, `class="badge failed"`); n != 2 {
		t.Errorf("home has %d parse-failed badges, want 2", n)
	}
	if !strings.Contains(home, never) || !strings.Contains(home, "good prompt") {
		t.Errorf("home is missing a failed Session:\n%s", home)
	}

	_, warn := get(t, srv.URL+"/?warnings=1")
	if strings.Contains(warn, "other prompt") || !strings.Contains(warn, never) || !strings.Contains(warn, "good prompt") {
		t.Errorf("has-warnings feed:\n%s", warn)
	}

	neverID := sessionID(t, home, never)
	code, page := get(t, srv.URL+"/sessions/"+neverID)
	if code != http.StatusOK || !strings.Contains(page, "parse failed") || !strings.Contains(page, "parser panic: boom") ||
		!strings.Contains(page, "never parsed") || strings.Contains(page, `class="msg`) {
		t.Errorf("never-parsed page (%d):\n%s", code, page)
	}

	goodID := sessionID(t, home, "good prompt")
	code, page = get(t, srv.URL+"/sessions/"+goodID)
	if code != http.StatusOK || !strings.Contains(page, "parser panic: boom") || !strings.Contains(page, "last Transcript that parsed") ||
		!strings.Contains(page, "good prompt") || strings.Contains(page, ">more<") {
		t.Errorf("failed page with a good Transcript (%d):\n%s", code, page)
	}
	if i, j := strings.Index(page, "parse failed"), strings.Index(page, `class="msg`); i < 0 || j < 0 || i > j {
		t.Error("failure note is not above the Transcript")
	}

	// Parsing counts the banner down.
	reg[protocol.SourceClaudeCode] = claudecode.New()
	if worked, _, err := w.RunOnce(ctx); !worked || err != nil {
		t.Fatal(worked, err)
	}
	if _, home := get(t, srv.URL+"/"); !strings.Contains(home, "Re-parsing 2 Sessions…") {
		t.Error("banner did not count down")
	}
	// An open page polls the banner, which keeps polling while jobs remain.
	if code, frag := get(t, srv.URL+"/reparse"); code != http.StatusOK || !strings.Contains(frag, "Re-parsing 2 Sessions…") ||
		!strings.Contains(frag, `hx-trigger="every 3s"`) {
		t.Errorf("banner poll (%d):\n%s", code, frag)
	}
	runAll()
	if _, home := get(t, srv.URL+"/"); strings.Contains(home, "Re-parsing") || strings.Contains(home, `class="badge failed"`) {
		t.Errorf("home after re-parse:\n%s", home)
	}
	// Once done, the poll answers 286 to stop htmx polling and offers a reload.
	if code, frag := get(t, srv.URL+"/reparse"); code != 286 || !strings.Contains(frag, "Re-parse finished") ||
		strings.Contains(frag, "hx-trigger") {
		t.Errorf("banner poll after re-parse (%d):\n%s", code, frag)
	}
}

// sessionID finds the id of the feed row containing text.
func sessionID(t *testing.T, home, text string) string {
	t.Helper()
	i := strings.Index(home, text)
	if i < 0 {
		t.Fatalf("%q not in feed", text)
	}
	j := strings.LastIndex(home[:i], `href="/sessions/`)
	if j < 0 {
		t.Fatalf("no row link before %q", text)
	}
	rest := home[j+len(`href="/sessions/`):]
	id := rest[:strings.IndexByte(rest, '"')]
	if _, err := strconv.Atoi(id); err != nil {
		t.Fatalf("row id %q", id)
	}
	return id
}
