// Package hermes is the Hub parser for the Hermes Agent Source
// (docs/spec/adapters/hermes.md §2.4, §3). Each Session is one database
// export: its sessions row, then its messages rows.
package hermes

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// version is the parser_version. Bump it whenever output changes for
// existing data (hub.md §4.5).
const version = 1

const layoutSqlite = "sqlite"

// subagentEntryPoint is the Hermes entry point of a Child Session.
const subagentEntryPoint = "subagent"

// Parser is the Hermes Agent parser.
type Parser struct{}

// New returns the Hermes Agent parser.
func New() *Parser { return &Parser{} }

// Source implements parser.Parser.
func (*Parser) Source() string { return protocol.SourceHermes }

// Version implements parser.Parser.
func (*Parser) Version() int { return version }

// keyPattern is a Hermes session id, such as 20261009_152518_0ba724.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// MapKey implements parser.Parser (hermes.md §2.4): the key is the session id.
func (*Parser) MapKey(key string) (parser.Mapping, bool) {
	if !keyPattern.MatchString(key) {
		return parser.Mapping{}, false
	}
	return parser.Mapping{NativeID: key, Role: parser.RoleMain, Layout: layoutSqlite, LayoutRank: 1}, true
}

// sessionRow is the sessions row's columns the parser reads.
type sessionRow struct {
	EntryPoint      string   `json:"source"` // the Hermes entry point, not our Source
	Title           *string  `json:"title"`
	Model           *string  `json:"model"`
	Cwd             *string  `json:"cwd"`
	GitRepoRoot     *string  `json:"git_repo_root"`
	GitBranch       *string  `json:"git_branch"`
	ParentSessionID *string  `json:"parent_session_id"`
	StartedAt       float64  `json:"started_at"`
	EndedAt         *float64 `json:"ended_at"`
	LastActivityAt  *float64 `json:"last_activity_at"`
}

// messageRow is a messages row's columns the parser reads.
type messageRow struct {
	ID                int64   `json:"id"`
	Role              string  `json:"role"`
	Content           *string `json:"content"`
	ToolCallID        *string `json:"tool_call_id"`
	ToolCalls         *string `json:"tool_calls"`
	ToolName          *string `json:"tool_name"`
	Timestamp         float64 `json:"timestamp"`
	Reasoning         *string `json:"reasoning"`
	ReasoningContent  *string `json:"reasoning_content"`
	CompressedSummary int64   `json:"_compressed_summary"`
	Active            int64   `json:"active"`
	Compacted         int64   `json:"compacted"`
	DisplayKind       *string `json:"display_kind"`
	DisplayOrder      *int64  `json:"display_order"`

	raw   []byte // the whole line
	order int64  // display_order, else id
}

// toolCall is one entry of an assistant row's tool_calls.
type toolCall struct {
	ID       string `json:"id"`
	CallID   string `json:"call_id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Parse implements parser.Parser.
func (*Parser) Parse(in parser.Input) (parser.Result, error) {
	var (
		res  parser.Result
		warn parser.Warnings
		s    *sessionRow
		sraw []byte
		rows []*messageRow
	)
	for raw := range bytes.Lines(in.Main) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var line struct {
			Table string          `json:"table"`
			Row   json.RawMessage `json:"row"`
		}
		if err := json.Unmarshal(raw, &line); err != nil {
			warn.Add(parser.WarnBadLine, "", string(raw))
			continue
		}
		switch line.Table {
		case "sessions":
			var r sessionRow
			if err := json.Unmarshal(line.Row, &r); err != nil {
				warn.Add(parser.WarnBadLine, "sessions", string(raw))
				continue
			}
			s, sraw = &r, raw
		case "messages":
			r := &messageRow{raw: raw}
			if err := json.Unmarshal(line.Row, r); err != nil {
				warn.Add(parser.WarnBadLine, "messages", string(raw))
				continue
			}
			rows = append(rows, r)
		default:
			// Any table a later export adds: Raw only.
		}
	}

	if s == nil {
		warn.Add(parser.WarnMissingField, "sessions", "")
		s = &sessionRow{}
	}
	res.Session = parser.Session{
		Title:     str(s.Title),
		StartedAt: millis(s.StartedAt),
		Cwd:       str(s.Cwd),
		GitBranch: str(s.GitBranch),
	}
	if s.EntryPoint == subagentEntryPoint {
		// Only a subagent is a Child Session. Compression continuations
		// and branches carry a parent too, but stand alone (hermes.md §3.5).
		res.Session.ParentNativeID = str(s.ParentSessionID)
	}
	if res.Session.Cwd == "" {
		res.Session.Cwd = str(s.GitRepoRoot)
	}
	if res.Session.Cwd == "" && sraw != nil {
		warn.Add(parser.WarnMissingField, "cwd", string(sraw))
	}
	last := s.StartedAt
	for _, t := range []*float64{s.EndedAt, s.LastActivityAt} {
		if t != nil {
			last = max(last, *t)
		}
	}

	// The Transcript is the shown rows: active, or compacted away but kept.
	// Rewound rows (neither) are Raw only (hermes.md §3.2).
	var shown []*messageRow
	for _, r := range rows {
		last = max(last, r.Timestamp)
		if r.Active == 0 && r.Compacted == 0 {
			continue
		}
		r.order = r.ID
		if r.DisplayOrder != nil {
			r.order = *r.DisplayOrder
		}
		shown = append(shown, r)
	}
	res.Session.LastActivityAt = millis(last)
	slices.SortStableFunc(shown, func(a, b *messageRow) int {
		return cmp.Or(cmp.Compare(a.order, b.order), cmp.Compare(a.ID, b.ID))
	})

	b := builder{res: &res, warn: &warn, model: str(s.Model), calls: map[string]*parser.ToolCallPayload{}}
	for _, r := range shown {
		b.row(r)
	}
	for _, m := range b.msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Calls were filled in through pointers as their results came.
		for i, p := range m.Parts {
			if tc, ok := p.Payload.(*parser.ToolCallPayload); ok {
				m.Parts[i].Payload = *tc
			}
		}
		res.Messages = append(res.Messages, *m)
	}
	res.Warnings = warn.List()
	return res, nil
}

// builder turns the shown rows into Messages, attaching each tool row to
// the call it answers.
type builder struct {
	res   *parser.Result
	warn  *parser.Warnings
	model string
	msgs  []*parser.Message
	calls map[string]*parser.ToolCallPayload // by call id, while unanswered
}

// row maps one shown row by role (hermes.md §3.3).
func (b *builder) row(r *messageRow) {
	id := strconv.FormatInt(r.ID, 10)
	m := &parser.Message{ID: id, Timestamp: millis(r.Timestamp)}
	part := func(kind string, payload any) {
		m.Parts = append(m.Parts, parser.Part{ID: id + "." + strconv.Itoa(len(m.Parts)), Kind: kind, Payload: payload})
	}
	if r.CompressedSummary != 0 {
		// The summary a compaction left in place of the rows it compacted.
		text := strings.TrimSpace(contentText(str(r.Content)))
		if text == "" {
			text = parser.CompactionText
		}
		m.Role = parser.MessageAssistant
		if r.Role == "user" {
			m.Role = parser.MessageUser
		}
		part(parser.KindMarker, parser.MarkerPayload{Marker: parser.MarkerCompaction, Text: text})
		b.msgs = append(b.msgs, m)
		return
	}
	if str(r.DisplayKind) == "hidden" {
		// Model-facing scaffolding Hermes never shows.
		return
	}
	switch r.Role {
	case "session_meta", "system":
		// Metadata, not Messages.
		return
	case "user":
		m.Role = parser.MessageUser
		b.content(r, part)
	case "assistant":
		m.Role = parser.MessageAssistant
		m.Model = b.model
		if t := str(r.Reasoning); t != "" {
			part(parser.KindThinking, parser.ThinkingPayload{Text: t})
		} else if t := str(r.ReasoningContent); t != "" {
			part(parser.KindThinking, parser.ThinkingPayload{Text: t})
		}
		b.content(r, part)
		if tc := str(r.ToolCalls); tc != "" {
			var calls []toolCall
			if err := json.Unmarshal([]byte(tc), &calls); err != nil {
				b.warn.Add(parser.WarnBadLine, "tool_calls", tc)
			}
			for _, c := range calls {
				p := newCall(c)
				part(parser.KindToolCall, p)
				b.calls[p.CallID] = p
			}
		}
	case "tool":
		b.result(r)
		return
	default:
		m.Role = parser.MessageAssistant
		part(parser.KindUnknown, parser.NewUnknown(r.Role, r.raw))
		b.warn.Add(parser.WarnUnknownType, r.Role, string(r.raw))
	}
	b.msgs = append(b.msgs, m)
}

// content adds a row's content: plain text, or Hermes's encoded list of
// multimodal parts.
func (b *builder) content(r *messageRow, part func(string, any)) {
	c := str(r.Content)
	enc, ok := strings.CutPrefix(c, contentJSONPrefix)
	if !ok {
		if strings.TrimSpace(c) != "" {
			part(parser.KindText, parser.TextPayload{Text: c})
		}
		return
	}
	var items []contentItem
	if err := json.Unmarshal([]byte(enc), &items); err != nil {
		// A single object, or something else: show it as text.
		if strings.TrimSpace(enc) != "" {
			part(parser.KindText, parser.TextPayload{Text: enc})
		}
		return
	}
	for _, it := range items {
		switch it.Type {
		case "text", "input_text":
			if strings.TrimSpace(it.Text) != "" {
				part(parser.KindText, parser.TextPayload{Text: it.Text})
			}
		case "image_url", "input_image":
			url := it.ImageURL.URL
			if url == "" {
				url = it.URL
			}
			if mime, data, ok := dataURL(url); ok {
				ip, img := parser.NewImage(mime, data)
				b.res.Images = append(b.res.Images, img)
				part(parser.KindImage, ip)
			} else {
				part(parser.KindAttachment, parser.AttachmentPayload{Label: "image"})
			}
		default:
			raw, _ := json.Marshal(it.raw)
			part(parser.KindUnknown, parser.NewUnknown(it.Type, raw))
			b.warn.Add(parser.WarnUnknownType, it.Type, string(raw))
		}
	}
}

// contentJSONPrefix marks content Hermes stored as JSON: a list of
// multimodal parts.
const contentJSONPrefix = "\x00json:"

// contentItem is one entry of a row's multimodal content list.
type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	URL      string `json:"url"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
	raw map[string]any
}

func (c *contentItem) UnmarshalJSON(b []byte) error {
	type plain contentItem
	if err := json.Unmarshal(b, (*plain)(c)); err != nil {
		return err
	}
	return json.Unmarshal(b, &c.raw)
}

// contentText is content's text, for a compaction summary.
func contentText(c string) string {
	enc, ok := strings.CutPrefix(c, contentJSONPrefix)
	if !ok {
		return c
	}
	var items []contentItem
	if json.Unmarshal([]byte(enc), &items) != nil {
		return enc
	}
	var texts []string
	for _, it := range items {
		if it.Text != "" {
			texts = append(texts, it.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// dataURL decodes a base64 data: URL.
func dataURL(u string) (mime string, data []byte, ok bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, false
	}
	meta, enc, ok := strings.Cut(rest, ",")
	mime, isB64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isB64 || !strings.HasPrefix(mime, "image/") {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	return mime, data, err == nil
}

// newCall is a pending tool_call Part for one of an assistant row's calls
// (hermes.md §3.4).
func newCall(c toolCall) *parser.ToolCallPayload {
	id := c.ID
	if id == "" {
		id = c.CallID
	}
	p := &parser.ToolCallPayload{CallID: id, Name: c.Function.Name, Status: parser.StatusPending, ChildSessions: []string{}}
	args := strings.TrimSpace(c.Function.Arguments)
	switch {
	case args == "":
		p.Input = json.RawMessage(`{}`)
	case json.Valid([]byte(args)):
		p.Input = json.RawMessage(args)
	default:
		p.Input, _ = json.Marshal(args)
	}
	if c.Function.Name == "patch" {
		var in struct {
			Path      string  `json:"path"`
			OldString *string `json:"old_string"`
			NewString *string `json:"new_string"`
		}
		if json.Unmarshal(p.Input, &in) == nil && in.OldString != nil && in.NewString != nil {
			p.Diff = &parser.Diff{Path: in.Path, Old: *in.OldString, New: *in.NewString}
		}
	}
	return p
}

// result attaches a tool row to the call it answers.
func (b *builder) result(r *messageRow) {
	call, ok := b.calls[str(r.ToolCallID)]
	if !ok {
		b.warn.Add(parser.WarnOrphan, "tool", string(r.raw))
		return
	}
	delete(b.calls, call.CallID)
	out := str(r.Content)
	call.Output = &out
	call.Status = parser.StatusOK
	if failed(out) {
		call.Status = parser.StatusError
	}
}

// failed reports whether a tool's output is a JSON object saying it
// failed: "success": false, or an "error" with no "success".
func failed(out string) bool {
	var o struct {
		Success *bool `json:"success"`
		Error   any   `json:"error"`
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") || json.Unmarshal([]byte(out), &o) != nil {
		return false
	}
	if o.Success != nil {
		return !*o.Success
	}
	e, isStr := o.Error.(string)
	return isStr && e != ""
}

// str is a nullable column's text, "" for NULL.
func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// millis is a Hermes time, float unix seconds, in Unix milliseconds.
func millis(f float64) int64 { return int64(math.Round(f * 1000)) }
