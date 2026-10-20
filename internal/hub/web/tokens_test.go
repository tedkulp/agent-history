package web

import (
	"strings"
	"testing"
	"time"
)

// tokensSpan opens the Transcript header's token figure.
const tokensSpan = `<span class="tokens"`

// assistantWithUsage is a Claude Code assistant line reporting Usage.
func assistantWithUsage(uuid, parent, usage string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","parentUuid":"` + parent + `","timestamp":"2026-09-01T10:00:01.000Z","message":{"id":"msg_` + uuid + `","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"usage":` + usage + `}}` + "\n"
}

// A Hermes Session's Session-level Usage shows as one compact figure, with
// every field's exact count on hover (hub.md §4.7).
func TestTranscriptHeaderShowsTokens(t *testing.T) {
	now := time.Now()
	srv := usageSite(t, record{"m1", "20261010_120000_aaaaaa", hmExport(t, "20261010_120000_aaaaaa", now.Add(-time.Hour), 1234567, 890)})

	_, page := get(t, srv.URL+"/sessions/1")
	want := `<span class="tokens" title="Input 1,234,567
Output 890
Cache read 0
Cache write 0
Reasoning 0">1.2M tokens</span>`
	if !strings.Contains(page, want) {
		t.Errorf("header lacks the token figure %q in:\n%s", want, page)
	}
}

// A Session with all-zero Usage, which counts as none, shows no figure.
func TestTranscriptHeaderWithoutUsage(t *testing.T) {
	srv := usageSite(t, record{"m1", "20261010_120000_bbbbbb", hmExport(t, "20261010_120000_bbbbbb", time.Now().Add(-time.Hour), 0, 0)})

	_, page := get(t, srv.URL+"/sessions/1")
	if !strings.Contains(page, `<div class="head">`) {
		t.Fatalf("no Transcript header in:\n%s", page)
	}
	if strings.Contains(page, tokensSpan) {
		t.Error("a Session without Usage shows a token figure")
	}
}

// The total adds all five fields, and a parent's figure leaves out its
// Child Sessions', which show their own.
func TestTranscriptHeaderTokensExcludeChildSessions(t *testing.T) {
	parent := line("u1", "", "user", `"go"`) +
		assistantWithUsage("a1", "u1", `{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":30,"cache_creation_input_tokens":4,"output_tokens_details":{"thinking_tokens":5}}`)
	child := `{"type":"user","uuid":"c1","parentUuid":null,"isSidechain":true,"timestamp":"2026-09-01T10:00:05.000Z","cwd":"/Users/ted/src/app","message":{"role":"user","content":"child task"}}` + "\n" +
		assistantWithUsage("c2", "c1", `{"input_tokens":2000000,"output_tokens":0}`)
	srv := newSite(t, map[string]string{
		"-Users-ted-src-app/" + sess + ".jsonl":                   parent,
		"-Users-ted-src-app/" + sess + "/subagents/agent-a.jsonl": child,
	})

	_, feed := get(t, srv.URL+"/")
	i := strings.Index(feed, `href="/sessions/`)
	if i < 0 {
		t.Fatal("feed lists no Session")
	}
	link := feed[i+len(`href="`):]
	link = link[:strings.Index(link, `"`)]

	_, page := get(t, srv.URL+link)
	if !strings.Contains(page, `">1.2k tokens</span>`) || !strings.Contains(page, "Reasoning 5") {
		t.Errorf("parent header lacks its own 1,239 tokens:\n%s", page)
	}
	i = strings.Index(page, `<a href="/sessions/`)
	if i < 0 {
		t.Fatal("parent lists no Child Session")
	}
	childLink := page[i+len(`<a href="`):]
	childLink = childLink[:strings.Index(childLink, `"`)]

	_, page = get(t, srv.URL+childLink)
	if !strings.Contains(page, `">2.0M tokens</span>`) {
		t.Errorf("Child Session header lacks its own 2.0M tokens:\n%s", page)
	}
}

// The live-update fragment's header carries the total as it grows.
func TestTranscriptTailCarriesTokens(t *testing.T) {
	ls := newLiveSite(t, line("u1", "", "user", `"first prompt"`))

	_, page := get(t, ls.srv.URL+"/sessions/1")
	if strings.Contains(page, tokensSpan) {
		t.Fatal("a Session without Usage shows a token figure")
	}
	after, count := state(t, page)

	ls.append(assistantWithUsage("a1", "u1", `{"input_tokens":52000,"output_tokens":700}`))
	ls.parse()

	_, upd := ls.tail(after, count)
	if !strings.Contains(upd, `">52.7k tokens</span>`) {
		t.Errorf("update lacks the new total:\n%s", upd)
	}
}
