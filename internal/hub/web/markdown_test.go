package web

import (
	"strings"
	"testing"
)

func TestMarkdownEscapesRawHTML(t *testing.T) {
	cases := []string{
		"hello <b>bold</b> <script>alert(1)</script>",
		"<div onclick=\"x()\">\nblock html\n</div>",
		"<!-- comment -->",
		"<img src=x onerror=alert(1)>",
	}
	for _, in := range cases {
		out := renderMarkdown(in)
		for _, bad := range []string{"<b>", "<script", "<div", "<!--", "<img"} {
			if strings.Contains(out, bad) {
				t.Errorf("renderMarkdown(%q) = %q contains %q", in, out, bad)
			}
		}
	}
	if out := renderMarkdown("a <b>b</b>"); !strings.Contains(out, "&lt;b&gt;b&lt;/b&gt;") {
		t.Errorf("inline HTML not shown as text: %q", out)
	}
	if out := renderMarkdown("<div>\nhi\n</div>"); !strings.Contains(out, "&lt;div&gt;") || !strings.Contains(out, "&lt;/div&gt;") {
		t.Errorf("block HTML not shown as text: %q", out)
	}
}

func TestMarkdownGFMAndHighlighting(t *testing.T) {
	out := renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |\n\n~~gone~~ https://example.com\n\n- [x] done\n\n```go\nfunc main() {}\n```\n")
	for _, want := range []string{"<table>", "<del>gone</del>", `<a href="https://example.com">`, `type="checkbox"`, `class="chroma"`, `<span class="kd">func</span>`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "style=") {
		t.Errorf("chroma used inline styles:\n%s", out)
	}
}

func TestMarkdownDropsDangerousLinks(t *testing.T) {
	if out := renderMarkdown("[x](javascript:alert(1))"); strings.Contains(out, "javascript:") {
		t.Errorf("dangerous link kept: %q", out)
	}
}
