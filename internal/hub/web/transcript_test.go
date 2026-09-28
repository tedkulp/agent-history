package web

import (
	"strings"
	"testing"
)

// A call's description, when it has one, leads its row; the field that would
// otherwise summarise it goes on a second line (#60).
func TestInputSummary(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 120) }
	for _, c := range []struct{ in, summary, second string }{
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
		summary, second := inputSummary([]byte(c.in))
		if summary != c.summary || second != c.second {
			t.Errorf("inputSummary(%s) = %q, %q; want %q, %q", c.in, summary, second, c.summary, c.second)
		}
	}
}
