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

func TestTitleCandidate(t *testing.T) {
	text := func(s string) Message {
		return Message{Role: MessageUser, Parts: []Part{{Kind: KindText, Payload: TextPayload{Text: s}}}}
	}
	cmd := func(s string) Message {
		return Message{Role: MessageUser, Parts: []Part{{Kind: KindMarker, Payload: MarkerPayload{Marker: MarkerSlashCommand, Text: s}}}}
	}
	reply := Message{Role: MessageAssistant, Parts: []Part{{Kind: KindText, Payload: TextPayload{Text: "sure"}}}}
	tests := []struct {
		name string
		msgs []Message
		want string
	}{
		{"prompt only", []Message{text("hello"), reply}, "hello"},
		{"housekeeping then command", []Message{cmd("/clear"), cmd("/implement-next 42"), reply}, "/implement-next 42"},
		{"command before prompt", []Message{cmd("/implement-next"), reply, text("yes, continue")}, "/implement-next"},
		{"prompt before command", []Message{text("fix it"), reply, cmd("/review")}, "fix it"},
		{"only housekeeping", []Message{cmd("/clear"), cmd("/model opus"), cmd("/compact")}, ""},
		{"marker without slash", []Message{cmd("review: foo"), text("go")}, "go"},
		{"assistant text is not a candidate", []Message{reply}, ""},
		{"other markers are not candidates", []Message{{Role: MessageUser, Parts: []Part{{Kind: KindMarker, Payload: MarkerPayload{Marker: MarkerShellCommand, Text: "/bin/ls"}}}}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TitleCandidate(tt.msgs); got != tt.want {
				t.Errorf("TitleCandidate = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTitleCandidateWithinOneMessage(t *testing.T) {
	m := Message{Role: MessageUser, Parts: []Part{
		{Kind: KindMarker, Payload: MarkerPayload{Marker: MarkerSlashCommand, Text: "/review 7"}},
		{Kind: KindText, Payload: TextPayload{Text: "look closely"}},
	}}
	if got := TitleCandidate([]Message{m}); got != "/review 7" {
		t.Errorf("TitleCandidate = %q, want the command before the text", got)
	}
	if got := FirstUserText([]Message{m}); got != "look closely" {
		t.Errorf("FirstUserText = %q, want the text only", got)
	}
}
