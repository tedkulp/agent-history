package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/protocol"
)

const (
	cxParent = "01a0a0c3-6416-72d2-ab61-cb1873b7787e"
	cxChild  = "01a0d390-1111-7222-8333-944455556666"
	cxFork   = "01a0d391-2222-7333-8444-955566667777"
)

func cxRollout(id, extra, prompt string) string {
	return `{"timestamp":"2026-09-24T13:08:26.379Z","ordinal":0,"type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-09-24T13:07:32.785Z","cwd":"/Users/ted/src/app","cli_version":"0.156.1","model_provider":"openai"` + extra + `}}
{"timestamp":"2026-09-24T13:08:27.000Z","ordinal":1,"type":"turn_context","payload":{"model":"gpt-6-sol","effort":"medium"}}
{"timestamp":"2026-09-24T13:08:28.000Z","ordinal":2,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"},{"type":"input_text","text":"` + prompt + `"}]}}
{"timestamp":"2026-09-24T13:08:29.000Z","ordinal":3,"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done."}]}}
`
}

// Codex Sessions show in the feed with their Source chip; a sub-agent
// thread is a Child Session linked to the top of its parent, and a fork
// shows what it was forked from (codex.md §6).
func TestCodexSessions(t *testing.T) {
	srv := newSiteSource(t, protocol.SourceCodex, []record{
		{"m1", "rollout-2026-09-24T09-07-32-" + cxParent + ".jsonl", cxRollout(cxParent, "", "Plan the work")},
		{"m1", "rollout-2026-09-24T09-30-00-" + cxChild + ".jsonl", cxRollout(cxChild, `,"parent_thread_id":"`+cxParent+`"`, "Explore the repo")},
		{"m1", "rollout-2026-09-24T10-00-00-" + cxFork + ".jsonl", cxRollout(cxFork, `,"forked_from_id":"`+cxParent+`"`, "Try another way")},
	})
	_, feed := get(t, srv.URL+"/")
	for _, want := range []string{"Plan the work", "Try another way", `<span class="badge codex">codex</span>`, "source=codex"} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed lacks %q", want)
		}
	}
	if strings.Contains(feed, "Explore the repo") {
		t.Error("the feed lists a Child Session")
	}
	if strings.Contains(feed, "environment_context") {
		t.Error("the feed shows injected context")
	}

	href := func(title string) string {
		for _, row := range strings.Split(feed, `<a class="row" href="`)[1:] {
			if strings.Contains(row, `<span class="t">`+title+`</span>`) {
				u, _, _ := strings.Cut(row, `"`)
				return u
			}
		}
		t.Fatalf("feed has no row for %q", title)
		return ""
	}
	parentURL, forkURL := href("Plan the work"), href("Try another way")

	_, parent := get(t, srv.URL+parentURL)
	c := regexp.MustCompile(`<a href="(/sessions/\d+)">↳ Explore the repo</a>`).FindStringSubmatch(parent)
	if c == nil {
		t.Fatal("the parent doesn't list its Child Session")
	}
	_, child := get(t, srv.URL+c[1])
	if !strings.Contains(child, `<a class="parent" href="`+parentURL+`">↰ child of Plan the work</a>`) {
		t.Error("the Child Session doesn't link to the top of its parent")
	}
	_, fork := get(t, srv.URL+forkURL)
	if !strings.Contains(fork, `<a class="fork" href="`+parentURL+`">forked from Plan the work</a>`) {
		t.Error("the fork doesn't show what it was forked from")
	}
	if !strings.Contains(fork, "gpt-6-sol") || !strings.Contains(fork, "Done.") {
		t.Error("the fork's Transcript is missing")
	}
}
