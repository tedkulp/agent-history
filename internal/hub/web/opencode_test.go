package web

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/protocol"
)

const (
	ocParent = "ses_0575ede0fffeQx7cLs3vMw9nTp"
	ocChild  = "ses_0575dd36bffeNw8xMq2vLs4cTp"
)

func ocFixture(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile("../parser/opencode/testdata/sqlite/" + id + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// opencode Sessions show in the feed from the database, their leftover
// legacy files shadowed; a tool error shows ✗, an edit a diff; the task
// call links to its Child Session and back; reverted Messages don't show;
// an unknown part shows as one (opencode.md §6).
func TestOpencodeSessions(t *testing.T) {
	s, _ := newHubSource(t, protocol.SourceOpencode, []record{
		{"m1", "json:session/proj/" + ocParent + ".json", `{"id":"` + ocParent + `","title":"Legacy title","directory":"/Users/ted/src/app","time":{"created":1,"updated":2}}`},
		{"m1", "json:message/" + ocParent + "/msg_00000000001LegacyMessage.json", `{"role":"user","time":{"created":1}}`},
		{"m1", "json:part/" + ocParent + "/msg_00000000001LegacyMessage/prt_00000000001LegacyPart.json", `{"type":"text","text":"Legacy words"}`},
		{"m1", "db:" + ocParent, ocFixture(t, ocParent)},
		{"m1", "db:" + ocChild, ocFixture(t, ocChild)},
	})
	srv := httptest.NewServer(New(s, nil, nil))
	t.Cleanup(srv.Close)

	_, feed := get(t, srv.URL+"/")
	for _, want := range []string{"Implement the design discussion", `<span class="badge opencode">opencode</span>`} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed lacks %q", want)
		}
	}
	for _, bad := range []string{"Legacy title", "Explore the remote architecture"} {
		if strings.Contains(feed, bad) {
			t.Errorf("feed shows %q", bad)
		}
	}
	if n := strings.Count(feed, `<a class="row" href="`); n != 1 {
		t.Errorf("feed has %d rows, want 1", n)
	}

	row := strings.Split(feed, `<a class="row" href="`)[1]
	parentURL, _, _ := strings.Cut(row, `"`)
	_, parent := get(t, srv.URL+parentURL)
	for _, want := range []string{
		"Implement the design from the discussion.",
		"question ✗",
		`<div class="diff-file">`,
		`<span class="del">- fmt.Println(&#34;hi&#34;)</span>`,
		"Error: MessageAbortedError: Aborted",
		"⚠ unknown <code>hologram</code>",
		`<summary><b>bash</b> <span class="dim">Run the tests</span> <span class="st ok">✓</span> <code class="target">go test ./...</code></summary>`,
	} {
		if !strings.Contains(parent, want) {
			t.Errorf("parent lacks %q", want)
		}
	}
	for _, bad := range []string{"Legacy words", "drop the second change", "Dropped.", "Called the Read tool"} {
		if strings.Contains(parent, bad) {
			t.Errorf("parent shows %q", bad)
		}
	}

	c := regexp.MustCompile(`<a href="(/sessions/\d+)">↳ Explore the remote architecture \(@explore subagent\)</a>`).FindStringSubmatch(parent)
	if c == nil {
		t.Fatal("the parent doesn't list its Child Session")
	}
	if !strings.Contains(parent, `<a class="child" href="`+c[1]+`">↳ Child Session</a>`) {
		t.Error("the task call doesn't link to its Child Session")
	}
	_, child := get(t, srv.URL+c[1])
	if !strings.Contains(child, `href="`+parentURL+`#p-prt_fa8a12873001Tn5cXe8kLb2mVq">↰ child of Implement the design discussion</a>`) {
		t.Error("the Child Session doesn't link back to its spawning call")
	}
}
