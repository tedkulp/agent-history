package web

import (
	"strings"
	"testing"
)

// A call's description, when it has one, leads its row; what it acts on goes on
// a second line (#60). The payload's description wins over the input's (#61).
func TestInputSummary(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 120) }
	for _, c := range []struct{ desc, in, summary, target string }{
		{"", `{"command":"go test ./...","description":"Run the tests"}`, "Run the tests", "go test ./..."},
		{"", `{"file_path":"a.go"}`, "a.go", ""},
		{"", `{"description":"Explore the code","prompt":"Look around"}`, "Explore the code", ""},
		{"", `{"description":"","command":"ls"}`, "ls", ""},
		{"", `{"description":" ","command":"ls"}`, "ls", ""},
		{"", `{"description":"List","command":"\n","path":"/src"}`, "List", "/src"},
		{" ", `{"command":"ls"}`, "ls", ""},
		{"", `{"description":"List\nfiles","path":"/src"}`, "List files", "/src"},
		{"", `{"description":"` + long("d") + `","command":"` + long("c") + `"}`, long("d")[:100] + "…", long("c")[:100] + "…"},
		{"", `{"prompt":"Look around"}`, "Look around", ""},
		{"", `{"x":1}`, `{"x":1}`, ""},
		{"", `{}`, "", ""},
		{"Reading the spec", `{"i":"Reading the spec","path":"docs/spec.md"}`, "Reading the spec", "docs/spec.md"},
		{"Running the tests", `{"command":"go test ./...","description":"Run the tests"}`, "Running the tests", "go test ./..."},
		{"Checking\nthe error", `{"code":"1/0"}`, "Checking the error", ""},
		{"Looking", `null`, "Looking", ""},
	} {
		summary, target := callSummary(c.desc, []byte(c.in))
		if summary != c.summary || target != c.target {
			t.Errorf("callSummary(%q, %s) = %q, %q; want %q, %q", c.desc, c.in, summary, target, c.summary, c.target)
		}
	}
}
