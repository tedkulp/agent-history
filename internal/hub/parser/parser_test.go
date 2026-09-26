package parser

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWarningExcerptKeepsValidUTF8(t *testing.T) {
	var w Warnings
	w.Add(WarnBadLine, "", strings.Repeat("a", 499)+"é tail")
	w.Add(WarnBadLine, "", "second")
	got := w.List()
	if len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("warnings = %+v", got)
	}
	if ex := got[0].FirstExcerpt; !utf8.ValidString(ex) || len(ex) > 500 {
		t.Errorf("excerpt is %d bytes, valid UTF-8 %v", len(ex), utf8.ValidString(ex))
	}
}
