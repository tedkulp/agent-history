package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

// stubOver is the output size above which a tool call's output is a stub
// loaded on click (hub.md §4.7).
const stubOver = 4 << 10

// chunkView is one rendered piece of a Message: one Part, or a cluster of
// consecutive tool_call Parts. Exactly one field group is set.
type chunkView struct {
	HTML       string // text: rendered Markdown
	Thinking   string // thinking: rendered Markdown
	Image      *imageView
	Marker     *markerView
	Unknown    *parser.UnknownPayload
	Attachment string     // attachment: its label
	Tools      []toolView // a cluster when non-empty
	Label      string     // the cluster's label
}

// markerView is a marker pill. Body, when set, opens below it.
type markerView struct {
	Kind  string
	Label string
	Body  string // rendered Markdown
}

// markerLabelMax is the most characters a marker pill shows.
const markerLabelMax = 120

type imageView struct {
	Src, Alt string
}

type toolView struct {
	ID        string // the Part id, as the row's anchor
	Name      string
	Summary   string // one line of input
	Status    string
	Input     string // pretty-printed JSON
	Diff      *diffView
	Output    string
	HasOutput bool
	Stub      string // "Output collapsed · 5.2 KB"; "" when inline
	Preview   string
	OutputURL string
}

type diffView struct {
	Path  string
	Lines []diffLine
}

type diffLine struct {
	Class string // add | del | ""
	Text  string
}

// chunks turns a Message's Parts into rendered chunks. Consecutive tool_call
// Parts fold into one cluster.
func (s *server) chunks(sessionID int64, parts []store.TranscriptPart) []chunkView {
	var out []chunkView
	for _, p := range parts {
		switch p.Kind {
		case parser.KindText:
			if tp, ok := decodePayload[parser.TextPayload](s, sessionID, p); ok {
				out = append(out, chunkView{HTML: renderMarkdown(tp.Text)})
			}
		case parser.KindThinking:
			if tp, ok := decodePayload[parser.ThinkingPayload](s, sessionID, p); ok {
				out = append(out, chunkView{Thinking: renderMarkdown(tp.Text)})
			}
		case parser.KindMarker:
			if mp, ok := decodePayload[parser.MarkerPayload](s, sessionID, p); ok {
				out = append(out, chunkView{Marker: newMarkerView(mp)})
			}
		case parser.KindUnknown:
			if up, ok := decodePayload[parser.UnknownPayload](s, sessionID, p); ok {
				out = append(out, chunkView{Unknown: &up})
			}
		case parser.KindAttachment:
			if ap, ok := decodePayload[parser.AttachmentPayload](s, sessionID, p); ok {
				out = append(out, chunkView{Attachment: ap.Label})
			}
		case parser.KindImage:
			var ip parser.ImagePayload
			if err := json.Unmarshal([]byte(p.Payload), &ip); err != nil || !shaRe.MatchString(ip.SHA256) {
				s.log.Warn("bad image payload", "session", sessionID, "part", p.ID, "err", err)
				continue
			}
			out = append(out, chunkView{Image: &imageView{Src: "/blobs/" + ip.SHA256, Alt: ip.Alt}})
		case parser.KindToolCall:
			var tc parser.ToolCallPayload
			if err := json.Unmarshal([]byte(p.Payload), &tc); err != nil {
				s.log.Warn("bad tool_call payload", "session", sessionID, "part", p.ID, "err", err)
				continue
			}
			tv := newToolView(sessionID, p.ID, tc)
			if n := len(out); n > 0 && out[n-1].Tools != nil {
				out[n-1].Tools = append(out[n-1].Tools, tv)
			} else {
				out = append(out, chunkView{Tools: []toolView{tv}})
			}
		}
	}
	for i := range out {
		if out[i].Tools != nil {
			out[i].Label = clusterLabel(out[i].Tools)
		}
	}
	return out
}

// decodePayload decodes a Part's payload, logging and skipping a bad one.
func decodePayload[T any](s *server, sessionID int64, p store.TranscriptPart) (T, bool) {
	var v T
	if err := json.Unmarshal([]byte(p.Payload), &v); err != nil {
		s.log.Warn("bad "+p.Kind+" payload", "session", sessionID, "part", p.ID, "err", err)
		return v, false
	}
	return v, true
}

// compactionSummaryLabel is the pill of a compaction marker holding the
// summary; the summary opens below it.
const compactionSummaryLabel = "Compaction summary"

// newMarkerView labels a marker pill. A compaction summary, and any marker
// text over one line, opens below the pill.
func newMarkerView(mp parser.MarkerPayload) *markerView {
	v := &markerView{Kind: mp.Marker, Label: mp.Text}
	if mp.Marker == parser.MarkerCompaction && mp.Text != parser.CompactionText {
		v.Label, v.Body = compactionSummaryLabel, renderMarkdown(mp.Text)
		return v
	}
	if first, _, multi := strings.Cut(mp.Text, "\n"); multi {
		v.Label, v.Body = cut(first, markerLabelMax), renderMarkdown(mp.Text)
	}
	if v.Label == "" {
		v.Label = mp.Marker
	}
	return v
}

func newToolView(sessionID int64, partID string, tc parser.ToolCallPayload) toolView {
	v := toolView{ID: partID, Name: tc.Name, Status: tc.Status, Summary: inputSummary(tc.Input), Input: prettyJSON(tc.Input)}
	if tc.Diff != nil {
		v.Diff = &diffView{Path: tc.Diff.Path, Lines: lineDiff(tc.Diff.Old, tc.Diff.New)}
	}
	switch {
	case tc.OutputSize > stubOver:
		v.HasOutput = true
		v.Stub = "Output collapsed · " + byteSize(tc.OutputSize) + " · click to load"
		v.Preview = tc.OutputPreview
		v.OutputURL = "/sessions/" + strconv.FormatInt(sessionID, 10) + "/parts/" + url.PathEscape(partID) + "/output"
	case tc.Output != nil && *tc.Output != "":
		v.HasOutput = true
		v.Output = *tc.Output
	}
	return v
}

// clusterLabel is "⚙ N tool calls · names", each name once in order of first
// use, marked ✗ when any of its calls failed.
func clusterLabel(tools []toolView) string {
	var (
		names  []string
		failed = map[string]bool{}
	)
	for _, t := range tools {
		if !slices.Contains(names, t.Name) {
			names = append(names, t.Name)
		}
		if t.Status == parser.StatusError {
			failed[t.Name] = true
		}
	}
	for i, n := range names {
		if failed[n] {
			names[i] = n + " ✗"
		}
	}
	calls := "tool calls"
	if len(tools) == 1 {
		calls = "tool call"
	}
	return fmt.Sprintf("⚙ %d %s · %s", len(tools), calls, strings.Join(names, ", "))
}

// summaryKeys are input fields that make a good one-line summary of a call.
var summaryKeys = []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"}

// inputSummary is one line describing a call's input: its most telling
// string field, else its compact JSON, cut to 100 characters.
func inputSummary(in json.RawMessage) string {
	var obj map[string]any
	if json.Unmarshal(in, &obj) == nil {
		for _, k := range summaryKeys {
			if v, ok := obj[k].(string); ok && v != "" {
				return cut(oneLine(v), 100)
			}
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, in) != nil {
		return ""
	}
	if s := buf.String(); s != "{}" && s != "null" {
		return cut(s, 100)
	}
	return ""
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func prettyJSON(in json.RawMessage) string {
	var buf bytes.Buffer
	if json.Indent(&buf, in, "", "  ") != nil {
		return string(in)
	}
	return buf.String()
}

// lineDiff is the line-level unified diff of old to new (hub.md §4.6).
func lineDiff(old, new string) []diffLine {
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(old, new)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)
	var out []diffLine
	for _, d := range diffs {
		class, sign := "", "  "
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			class, sign = "add", "+ "
		case diffmatchpatch.DiffDelete:
			class, sign = "del", "- "
		}
		for _, l := range strings.SplitAfter(d.Text, "\n") {
			if l == "" {
				continue
			}
			out = append(out, diffLine{Class: class, Text: sign + strings.TrimSuffix(l, "\n")})
		}
	}
	return out
}

func byteSize(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// toolOutput serves the full output of one tool call as an htmx fragment.
func (s *server) toolOutput(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, ok, err := s.store.ToolOutput(r.Context(), id, r.PathValue("part"))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, toolOutputFragment(out))
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// inlineImageTypes are the MIME types served as themselves. Anything else,
// SVG included, is served as a download so it can't run script.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// blob serves an image blob, cached forever since its URL is its hash.
func (s *server) blob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRe.MatchString(sha) {
		http.NotFound(w, r)
		return
	}
	mime, b, ok, err := s.store.Blob(r.Context(), sha)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !inlineImageTypes[mime] {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(b)
}
