package web

import (
	"strings"
	"testing"
)

// A call's description, when it has one, leads its row; what it acts on goes on
// a second line (#60).
func TestInputSummary(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 120) }
	for _, c := range []struct{ in, summary, target string }{
		{`{"command":"go test ./...","description":"Run the tests"}`, "Run the tests", "go test ./..."},
		{`{"file_path":"a.go"}`, "a.go", ""},
		{`{"description":"Explore the code","prompt":"Look around"}`, "Explore the code", ""},
		{`{"description":"","command":"ls"}`, "ls", ""},
		{`{"description":"List\nfiles","path":"/src"}`, "List files", "/src"},
		{`{"description":"` + long("d") + `","command":"` + long("c") + `"}`, long("d")[:100] + "…", long("c")[:100] + "…"},
		{`{"prompt":"Look around"}`, "Look around", ""},
		{`{"x":1}`, `{"x":1}`, ""},
		{`{}`, "", ""},
	} {
		summary, target := inputSummary([]byte(c.in))
		if summary != c.summary || target != c.target {
			t.Errorf("inputSummary(%s) = %q, %q; want %q, %q", c.in, summary, target, c.summary, c.target)
		}
	}
}
