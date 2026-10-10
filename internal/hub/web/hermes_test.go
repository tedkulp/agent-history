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
	hmParent = "20261002_140000_a1b2c3"
	hmChild  = "20261002_140100_c0ffee"
)

func hmFixture(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile("../parser/hermes/testdata/" + id + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Hermes Sessions show in the feed with their Source chip and badge; the
// Transcript keeps compacted rows, drops the rewound one, and lists the
// subagent Session as a Child Session that links back (hermes.md §6).
func TestHermesSessions(t *testing.T) {
	s, _ := newHubSource(t, protocol.SourceHermes, []record{
		{"m1", hmParent, hmFixture(t, hmParent)},
		{"m1", hmChild, hmFixture(t, hmChild)},
	})
	srv := httptest.NewServer(New(s, nil, nil))
	t.Cleanup(srv.Close)

	_, feed := get(t, srv.URL+"/")
	for _, want := range []string{"Fix the login bug", `<span class="badge hermes">hermes</span>`, "?source=hermes"} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed lacks %q", want)
		}
	}
	if n := strings.Count(feed, `<a class="row" href="`); n != 1 {
		t.Errorf("feed has %d rows, want 1", n)
	}

	row := strings.Split(feed, `<a class="row" href="`)[1]
	parentURL, _, _ := strings.Cut(row, `"`)
	_, parent := get(t, srv.URL+parentURL)
	for _, want := range []string{"Why does login fail?", "Summary: the login handler ignores the session cookie.", "Fixed: the handler now checks the cookie."} {
		if !strings.Contains(parent, want) {
			t.Errorf("parent lacks %q", want)
		}
	}
	for _, bad := range []string{"Actually, never mind", "(continuing)"} {
		if strings.Contains(parent, bad) {
			t.Errorf("parent shows %q", bad)
		}
	}

	c := regexp.MustCompile(`<a href="(/sessions/\d+)">↳ Review the patch</a>`).FindStringSubmatch(parent)
	if c == nil {
		t.Fatal("the parent doesn't list its Child Session")
	}
	_, child := get(t, srv.URL+c[1])
	if !strings.Contains(child, `href="`+parentURL) {
		t.Error("the Child Session doesn't link back to its parent")
	}
}
