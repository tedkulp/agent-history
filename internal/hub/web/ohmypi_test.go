package web

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

const ompSession = "-Users-ted-src-app/2026-09-17T17-58-38-183Z_01a0b085-1516-7000-9081-e596e495299d"

// ompTitleSlot is omp's fixed-width title slot holding title.
func ompTitleSlot(title string) string {
	line := `{"type":"title","v":1,"title":"` + title + `","updatedAt":"2026-09-17T18:13:24.506Z","pad":"`
	return line + strings.Repeat(" ", 120-len(line)) + `"}` + "\n"
}

const ompMain = `{"type":"session","version":3,"id":"01a0b085-1516-7000-9081-e596e495299d","timestamp":"2026-09-17T17:58:38.183Z","cwd":"/Users/ted/src/app"}
{"type":"message","id":"a0000001","parentId":null,"timestamp":"2026-09-17T17:58:40.000Z","message":{"role":"user","content":"Research the switch"}}
{"type":"message","id":"a0000002","parentId":"a0000001","timestamp":"2026-09-17T17:58:50.000Z","message":{"role":"assistant","model":"gpt-5.6-sol","provider":"openai-codex","content":[{"type":"toolCall","id":"call_task|fc_1","name":"task","arguments":{"tasks":[{"name":"Alpha"},{"name":"Beta"}]}}]}}
{"type":"message","id":"a0000003","parentId":"a0000002","timestamp":"2026-09-17T17:58:51.000Z","message":{"role":"toolResult","toolCallId":"call_task|fc_1","toolName":"task","content":[{"type":"text","text":"Spawned 2 background agents"}],"details":{"results":[],"progress":[{"id":"Alpha"},{"id":"Beta"}]},"isError":false}}
`

func ompChild(task string) string {
	return ompTitleSlot("") + `{"type":"session","version":3,"id":"01a0b099-2222-7333-8444-955566667777","timestamp":"2026-09-17T17:58:52.000Z","cwd":"/Users/ted/src/app"}
{"type":"session_init","id":"d0000001","parentId":null,"timestamp":"2026-09-17T17:58:52.000Z","task":"` + task + `","agent":"explore"}
{"type":"message","id":"d0000002","parentId":"d0000001","timestamp":"2026-09-17T17:58:52.000Z","message":{"role":"user","content":[{"type":"text","text":"` + task + `"}]}}
{"type":"message","id":"d0000003","parentId":"d0000002","timestamp":"2026-09-17T17:58:59.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Found it."}]}}
`
}

// oh-my-pi Sessions show in the feed; a retitled Session, shipped as a
// replace, shows its new title; a task call links to both sub-agents and
// each links back (oh-my-pi.md §6).
func TestOhMyPiSessions(t *testing.T) {
	s, drain := newHubSource(t, protocol.SourceOhMyPi, []record{
		{"m1", ompSession + ".jsonl", ompTitleSlot("Plan the work") + ompMain},
		{"m1", ompSession + "/Alpha.jsonl", ompChild("Look at alpha")},
		{"m1", ompSession + "/Beta.jsonl", ompChild("Look at beta")},
	})
	// omp rewrites the title slot in place; the Collector ships a replace.
	// Live parses of one Session are spaced out, so move past that.
	s.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	body := ompTitleSlot("Switch researched") + ompMain
	enc, _ := zstd.NewWriter(nil)
	if _, err := s.Replace(context.Background(), store.ReplaceRequest{
		MachineID: "m1", Source: protocol.SourceOhMyPi, RecordKey: ompSession + ".jsonl",
		Data: []byte(body), Compressed: enc.EncodeAll([]byte(body), nil),
	}); err != nil {
		t.Fatal(err)
	}
	drain()
	srv := httptest.NewServer(New(s, nil, nil))
	t.Cleanup(srv.Close)

	_, feed := get(t, srv.URL+"/")
	for _, want := range []string{"Switch researched", `<span class="badge oh-my-pi">oh-my-pi</span>`} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed lacks %q", want)
		}
	}
	for _, bad := range []string{"Plan the work", "Look at alpha", "explore: Alpha"} {
		if strings.Contains(feed, bad) {
			t.Errorf("feed shows %q", bad)
		}
	}

	row := strings.Split(feed, `<a class="row" href="`)[1]
	parentURL, _, _ := strings.Cut(row, `"`)
	_, parent := get(t, srv.URL+parentURL)
	for _, name := range []string{"Alpha", "Beta"} {
		c := regexp.MustCompile(`<a href="(/sessions/\d+)">↳ explore: ` + name + `</a>`).FindStringSubmatch(parent)
		if c == nil {
			t.Fatalf("the parent doesn't list %s", name)
		}
		if !strings.Contains(parent, `<a class="child" href="`+c[1]+`">↳ Child Session</a>`) {
			t.Errorf("the task call doesn't link to %s", name)
		}
		_, child := get(t, srv.URL+c[1])
		if !strings.Contains(child, `href="`+parentURL+`#p-a0000002.0">↰ child of Switch researched</a>`) {
			t.Errorf("%s doesn't link back to its spawning call", name)
		}
		// Once as a Message and once in the outline, not twice as Messages.
		if n := strings.Count(child, "Look at "+strings.ToLower(name)); n != 2 {
			t.Errorf("%s shows its task %d times", name, n)
		}
	}
}
