// Package codex is the Hub parser for the codex Source
// (docs/spec/adapters/codex.md §2.3, §3).
package codex

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// version is the parser_version. Bump it whenever output changes for
// existing data (hub.md §4.5).
const version = 1

const (
	layout     = "jsonl"
	layoutRank = 1
)

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// rolloutRe matches a Record key: rollout-<ts>-<thread>[_<rollout>].jsonl.
var rolloutRe = regexp.MustCompile(`^rollout-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})-(` + uuid + `)(?:_(` + uuid + `))?\.jsonl$`)

// Parser is the codex parser.
type Parser struct{}

// New returns the codex parser.
func New() *Parser { return &Parser{} }

// Source implements parser.Parser.
func (*Parser) Source() string { return protocol.SourceCodex }

// Version implements parser.Parser.
func (*Parser) Version() int { return version }

// MapKey implements parser.Parser (codex.md §2.3): a Session's first rollout
// is its main record, continuation files are attachments.
func (*Parser) MapKey(key string) (parser.Mapping, bool) {
	k, ok := parseKey(key)
	if !ok {
		return parser.Mapping{}, false
	}
	role := parser.RoleMain
	if k.continuation {
		role = parser.RoleAttachment
	}
	return parser.Mapping{NativeID: k.thread, Role: role, Layout: layout, LayoutRank: layoutRank}, true
}

// rolloutKey is what a Record key names.
type rolloutKey struct {
	ts           string // the creation time
	thread       string // the Session's native id
	rollout      string // the file's rollout id: its _<rollout>, else the thread id
	continuation bool
}

func parseKey(key string) (rolloutKey, bool) {
	m := rolloutRe.FindStringSubmatch(key)
	if m == nil {
		return rolloutKey{}, false
	}
	k := rolloutKey{ts: m[1], thread: m[2], rollout: m[2], continuation: m[3] != ""}
	if k.continuation {
		k.rollout = m[3]
	}
	return k, true
}

// line is one rollout line.
type line struct {
	Timestamp string          `json:"timestamp"`
	Ordinal   *uint64         `json:"ordinal"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`

	raw []byte
	ts  int64
	ord uint64
	key string // the Record key of the file holding it
	dup bool   // another stitched file has the same ordinal
}

// sessionMeta is the part of session_meta the parser reads (codex.md §3.5).
type sessionMeta struct {
	ID                          string       `json:"id"`
	Timestamp                   string       `json:"timestamp"`
	Cwd                         string       `json:"cwd"`
	CLIVersion                  string       `json:"cli_version"`
	ModelProvider               string       `json:"model_provider"`
	ParentThreadID              string       `json:"parent_thread_id"`
	ForkedFromID                string       `json:"forked_from_id"`
	HistoryBase                 *historyBase `json:"history_base"`
	SubagentHistoryStartOrdinal *uint64      `json:"subagent_history_start_ordinal"`
	Git                         *struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

// historyBase is where a paginated rollout continues another: ThreadID is
// the prefix file's rollout id, whose lines before EndOrdinalExclusive come
// first (codex-rs HistoryPosition).
type historyBase struct {
	ThreadID            string `json:"thread_id"`
	EndOrdinalExclusive uint64 `json:"end_ordinal_exclusive"`
}

// file is one of a Session's rollout files.
type file struct {
	key     string
	ts      string // <ts> from the key
	rollout string // the rollout id: the key's _<rollout>, else the thread id
	meta    *sessionMeta
	lines   []*line
}

// Parse implements parser.Parser.
func (*Parser) Parse(in parser.Input) (parser.Result, error) {
	var (
		res  parser.Result
		warn parser.Warnings
	)
	files := []*file{readFile(in.MainKey, in.Main, &warn)}
	keys := make([]string, 0, len(in.Attachments))
	for k := range in.Attachments {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		files = append(files, readFile(k, in.Attachments[k], &warn))
	}

	main := files[0]
	meta := main.meta
	if meta == nil {
		warn.Add(parser.WarnMissingField, "session_meta", "")
		meta = &sessionMeta{}
	}
	if meta.ID != "" && meta.ID != in.NativeID {
		warn.Add(parser.WarnMissingField, "id", meta.ID)
	}
	res.Session = parser.Session{
		StartedAt:          parseTime(meta.Timestamp),
		Cwd:                meta.Cwd,
		SourceVersion:      meta.CLIVersion,
		ParentNativeID:     meta.ParentThreadID,
		ForkedFromNativeID: meta.ForkedFromID,
	}
	if meta.Git != nil {
		res.Session.GitBranch = meta.Git.Branch
	}
	if res.Session.Cwd == "" {
		warn.Add(parser.WarnMissingField, "cwd", "")
	}

	lines := stitch(files)
	for _, l := range lines {
		if l.ts > res.Session.LastActivityAt {
			res.Session.LastActivityAt = l.ts
		}
	}
	if res.Session.StartedAt == 0 && len(lines) > 0 {
		res.Session.StartedAt = lines[0].ts
	}
	b := &builder{warn: &warn, provider: meta.ModelProvider, outputs: map[string]*output{}, responses: map[string]bool{}}
	if s := meta.SubagentHistoryStartOrdinal; s != nil {
		// Inherited parent context is Raw only (codex.md §3.2), but its
		// last turn_context still sets the model.
		for _, l := range lines {
			if l.ord < *s && l.Type == "turn_context" {
				b.turnContext(l)
			}
		}
		b.msgs, b.seenTurn = nil, false
		lines = slices.DeleteFunc(lines, func(l *line) bool { return l.ord < *s })
	}

	b.build(lines)
	res.Messages, res.Images = b.messages(), b.images
	res.Warnings = warn.List()
	return res, nil
}

// readFile decodes one rollout. Its first line is session_meta.
func readFile(key string, content []byte, warn *parser.Warnings) *file {
	f := &file{key: key}
	if k, ok := parseKey(key); ok {
		f.ts, f.rollout = k.ts, k.rollout
	}
	n := uint64(0)
	for raw := range bytes.Lines(content) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		l := &line{raw: raw, key: key}
		if err := json.Unmarshal(raw, l); err != nil {
			warn.Add(parser.WarnBadLine, "", string(raw))
			continue
		}
		l.ts = parseTime(l.Timestamp)
		// Rollouts written before ordinals existed count lines instead.
		l.ord = n
		if l.Ordinal != nil {
			l.ord = *l.Ordinal
		}
		n++
		if l.Type == "session_meta" && f.meta == nil && len(f.lines) == 0 {
			var m sessionMeta
			if json.Unmarshal(l.Payload, &m) == nil {
				f.meta = &m
			}
		}
		f.lines = append(f.lines, l)
	}
	return f
}

// parseTime is an RFC 3339 time in Unix milliseconds, 0 when it doesn't parse.
func parseTime(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// segment is one file's part of a stitched Transcript.
type segment struct {
	f   *file
	end *uint64 // lines from end on belong to the next segment
}

// stitch orders a Session's files, the main one first, and returns their
// lines in ordinal order (codex.md §3.2). The current file is the one no
// other continues from; its history_base chain leads back through the files
// it continues, each taken up to where the next continues from. Files off
// that chain were reverted away. When there is no chain back to the main
// file, files follow <ts> order.
func stitch(files []*file) []*line {
	if len(files) == 1 {
		return files[0].lines
	}
	segs, ok := chain(files)
	if !ok {
		sorted := slices.Clone(files)
		slices.SortFunc(sorted, byTime)
		segs = nil
		for _, f := range sorted {
			segs = append(segs, segment{f: f})
		}
	}

	byOrd := map[uint64]*line{}
	for i, s := range segs {
		for j, l := range s.f.lines {
			if s.end != nil && l.ord >= *s.end {
				continue
			}
			if i > 0 && j == 0 && l.Type == "session_meta" {
				// A continuation's own session_meta is Raw only.
				continue
			}
			if prev, ok := byOrd[l.ord]; ok && prev.key != l.key {
				l.dup = true
			}
			byOrd[l.ord] = l
		}
	}
	out := make([]*line, 0, len(byOrd))
	for _, l := range byOrd {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b *line) int { return cmp.Compare(a.ord, b.ord) })
	return out
}

// chain follows history_base back from the current file. ok is false when
// no chain leads back to the main file, files[0].
func chain(files []*file) (segs []segment, ok bool) {
	byRollout := map[string]*file{}
	continued := map[string]bool{}
	for _, f := range files {
		byRollout[f.rollout] = f
		if b := base(f); b != nil {
			continued[b.ThreadID] = true
		}
	}
	var heads []*file
	for _, f := range files {
		if !continued[f.rollout] {
			heads = append(heads, f)
		}
	}
	if len(heads) == 0 {
		return nil, false
	}
	slices.SortFunc(heads, byTime)
	seen := map[*file]bool{}
	var end *uint64
	for f := heads[len(heads)-1]; f != nil && !seen[f]; {
		seen[f] = true
		segs = append(segs, segment{f, end})
		b := base(f)
		if b == nil {
			break
		}
		e := b.EndOrdinalExclusive
		end = &e
		f = byRollout[b.ThreadID]
	}
	if !seen[files[0]] {
		return nil, false
	}
	slices.Reverse(segs)
	return segs, true
}

// byTime orders files by the <ts> in their keys.
func byTime(a, b *file) int { return cmp.Or(cmp.Compare(a.ts, b.ts), cmp.Compare(a.key, b.key)) }

// base is where f continues another file, or nil.
func base(f *file) *historyBase {
	if f.meta == nil {
		return nil
	}
	return f.meta.HistoryBase
}

// msg is a Message being built. Parts get their ids at the end, once images
// from tool output have been placed after their calls.
type msg struct {
	parser.Message
	parts []*part
}

type part struct {
	kind    string
	payload any
	call    *parser.ToolCallPayload // for a tool_call
	images  []parser.ImagePayload   // from the call's output, placed after it
}

// output is one *_output item, matched to its call by call_id.
type output struct {
	text   string
	images []parser.ImagePayload
	failed bool // the output says success: false
	raw    []byte
	used   bool
	ord    uint64
}

// builder turns lines into Messages (codex.md §3.3, §3.4).
type builder struct {
	warn     *parser.Warnings
	provider string
	model    string
	effort   string
	seenTurn bool

	msgs      []*msg
	cur       *msg // the assistant Message being grouped
	lastAsst  *msg // the latest assistant Message with Parts, for usage
	outputs   map[string]*output
	responses map[string]bool // response ids already counted
	images    []parser.Image
}

// item is the payload of a response_item: the union of the fields the
// parser reads from each item type.
type item struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Status    string          `json:"status"`
	Output    json.RawMessage `json:"output"`
}

// block is one content block of a message or output.
type block struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`

	raw json.RawMessage
}

// build turns lines into Messages, then warns about outputs no call claimed.
func (b *builder) build(lines []*line) {
	// Outputs first, so each call finds its output wherever it lies.
	for _, l := range lines {
		if l.Type != "response_item" {
			continue
		}
		var it item
		if json.Unmarshal(l.Payload, &it) != nil {
			continue
		}
		if it.Type == "function_call_output" || it.Type == "custom_tool_call_output" {
			if _, dup := b.outputs[it.CallID]; dup || it.CallID == "" {
				// No call can claim it.
				b.warn.Add(parser.WarnOrphan, it.Type, string(l.Payload))
				continue
			}
			b.outputs[it.CallID] = b.readOutput(it, l)
		}
	}

	for _, l := range lines {
		switch l.Type {
		case "session_meta", "event_msg", "world_state":
			// Raw only.
		case "turn_context":
			b.flush()
			b.turnContext(l)
		case "response_item":
			b.responseItem(l)
		case "token_usage_record":
			b.usage(l)
		case "compacted":
			b.flush()
			var c struct {
				Message string `json:"message"`
			}
			text := parser.CompactionText
			if json.Unmarshal(l.Payload, &c) == nil && strings.TrimSpace(c.Message) != "" {
				text = c.Message
			}
			b.marker(l, parser.MarkerPayload{Marker: parser.MarkerCompaction, Text: text})
		default:
			b.flush()
			b.warn.Add(parser.WarnUnknownType, l.Type, string(l.raw))
			m := b.newMsg(l, parser.MessageAssistant)
			m.parts = append(m.parts, &part{kind: parser.KindUnknown, payload: parser.NewUnknown(l.Type, l.raw)})
			b.msgs = append(b.msgs, m)
		}
	}
	b.flush()

	var orphans []*output
	for _, o := range b.outputs {
		if !o.used {
			orphans = append(orphans, o)
		}
	}
	slices.SortFunc(orphans, func(x, y *output) int { return cmp.Compare(x.ord, y.ord) })
	for _, o := range orphans {
		var it item
		json.Unmarshal(o.raw, &it)
		b.warn.Add(parser.WarnOrphan, it.Type, string(o.raw))
	}
}

// turnContext sets the model for following Messages and marks a change of
// model or effort from the previous turn_context.
func (b *builder) turnContext(l *line) {
	var tc struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}
	if json.Unmarshal(l.Payload, &tc) != nil {
		b.warn.Add(parser.WarnMissingField, "turn_context", string(l.raw))
		return
	}
	if b.seenTurn {
		if tc.Model != b.model && tc.Model != "" {
			b.marker(l, parser.MarkerPayload{Marker: parser.MarkerModelChange, Text: tc.Model})
		}
		if tc.Effort != b.effort && tc.Effort != "" {
			b.marker(l, parser.MarkerPayload{Marker: parser.MarkerThinkingLevel, Text: tc.Effort})
		}
	}
	b.seenTurn = true
	if tc.Model != "" {
		b.model = tc.Model
	}
	if tc.Effort != "" {
		b.effort = tc.Effort
	}
}

// marker appends a Message holding one marker Part. Two markers from one
// line share the line's Message.
func (b *builder) marker(l *line, mp parser.MarkerPayload) {
	p := &part{kind: parser.KindMarker, payload: mp}
	if n := len(b.msgs); n > 0 && b.msgs[n-1].ID == msgID(l) && b.msgs[n-1].parts[0].kind == parser.KindMarker {
		b.msgs[n-1].parts = append(b.msgs[n-1].parts, p)
		return
	}
	m := b.newMsg(l, parser.MessageAssistant)
	m.parts = append(m.parts, p)
	b.msgs = append(b.msgs, m)
}

// msgID is the id of a Message starting at l (codex.md §3.6).
func msgID(l *line) string {
	if !l.dup {
		return strconv.FormatUint(l.ord, 10)
	}
	sum := sha256.Sum256([]byte(l.key + "\x00" + strconv.FormatUint(l.ord, 10)))
	return hex.EncodeToString(sum[:8])
}

// newMsg starts a Message at l.
func (b *builder) newMsg(l *line, role string) *msg {
	return &msg{Message: parser.Message{ID: msgID(l), Role: role, Timestamp: l.ts}}
}

// flush ends the assistant Message being grouped.
func (b *builder) flush() {
	if b.cur != nil && len(b.cur.parts) > 0 {
		b.msgs = append(b.msgs, b.cur)
		b.lastAsst = b.cur
	}
	b.cur = nil
}

// assistant returns the assistant Message being grouped, starting one at l.
func (b *builder) assistant(l *line) *msg {
	if b.cur == nil {
		b.cur = b.newMsg(l, parser.MessageAssistant)
		b.cur.Model, b.cur.Provider = b.model, b.provider
	}
	return b.cur
}

// responseItem maps one response_item (codex.md §3.3).
func (b *builder) responseItem(l *line) {
	var it item
	if json.Unmarshal(l.Payload, &it) != nil {
		b.warn.Add(parser.WarnMissingField, "payload", string(l.raw))
		return
	}
	switch it.Type {
	case "message":
		switch it.Role {
		case "user":
			b.userMessage(l, it)
		case "developer", "system":
			// Instructions: Raw only.
		case "assistant":
			m := b.assistant(l)
			for _, bl := range blocks(it.Content) {
				if bl.Type == "output_text" {
					m.parts = append(m.parts, &part{kind: parser.KindText, payload: parser.TextPayload{Text: bl.Text}})
				} else {
					b.unknown(m, bl.Type, bl.raw)
				}
			}
		default:
			b.unknown(b.assistant(l), "message:"+it.Role, l.Payload)
		}
	case "reasoning":
		var sum []struct {
			Text string `json:"text"`
		}
		json.Unmarshal(it.Summary, &sum)
		var texts []string
		for _, s := range sum {
			if s.Text != "" {
				texts = append(texts, s.Text)
			}
		}
		if len(texts) > 0 {
			m := b.assistant(l)
			m.parts = append(m.parts, &part{kind: parser.KindThinking, payload: parser.ThinkingPayload{Text: strings.Join(texts, "\n\n")}})
		}
	case "function_call", "custom_tool_call":
		b.toolCall(b.assistant(l), it)
	case "function_call_output", "custom_tool_call_output":
		// Merged into their call.
	default:
		b.unknown(b.assistant(l), it.Type, l.Payload)
	}
}

// unknown adds an unknown Part with an unknown_type warning.
func (b *builder) unknown(m *msg, sourceType string, raw []byte) {
	b.warn.Add(parser.WarnUnknownType, sourceType, string(raw))
	m.parts = append(m.parts, &part{kind: parser.KindUnknown, payload: parser.NewUnknown(sourceType, raw)})
}

// openTagRe matches the opening tag at the start of a block.
var openTagRe = regexp.MustCompile(`^<([A-Za-z][\w-]*)(?:\s[^>]*)?>`)

// injected reports whether a block is one <tag>…</tag> element: context
// Codex adds to the user's turn (codex.md §3.3). The element's first closing
// tag must end the block, so "<a>x</a> text <a>y</a>" is the user's own.
func injected(text string) bool {
	text = strings.TrimSpace(text)
	m := openTagRe.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	body := text[len(m[0]):]
	i := strings.Index(body, "</"+m[1])
	if i < 0 {
		return false
	}
	return closeTagRestRe.MatchString(body[i+len("</"+m[1]):])
}

// closeTagRestRe is what may follow a closing tag's name to end a block.
var closeTagRestRe = regexp.MustCompile(`^(?:\s[^>]*)?>$`)

// userMessage adds a user Message, leaving out injected context. A message
// with nothing else adds none.
func (b *builder) userMessage(l *line, it item) {
	m := b.newMsg(l, parser.MessageUser)
	for _, bl := range blocks(it.Content) {
		switch bl.Type {
		case "input_text":
			if !injected(bl.Text) {
				m.parts = append(m.parts, &part{kind: parser.KindText, payload: parser.TextPayload{Text: bl.Text}})
			}
		case "input_image":
			if p, ok := b.image(bl, l.raw); ok {
				m.parts = append(m.parts, &part{kind: parser.KindImage, payload: p})
			}
		default:
			b.unknown(m, bl.Type, bl.raw)
		}
	}
	if len(m.parts) == 0 {
		return
	}
	b.flush()
	b.msgs = append(b.msgs, m)
}

// image decodes an inline data: URL image. Other references carry no
// bytes: they are skipped with an unknown_type warning.
func (b *builder) image(bl block, raw []byte) (parser.ImagePayload, bool) {
	rest, isData := strings.CutPrefix(bl.ImageURL, "data:")
	mime, data, isBase64 := strings.Cut(rest, ";base64,")
	if !isData || !isBase64 {
		b.warn.Add(parser.WarnUnknownType, "input_image", string(raw))
		return parser.ImagePayload{}, false
	}
	bs, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		b.warn.Add(parser.WarnMissingField, "input_image.image_url", string(raw))
		return parser.ImagePayload{}, false
	}
	p, img := parser.NewImage(mime, bs)
	b.images = append(b.images, img)
	return p, true
}

// readOutput reads an output item: a string as-is, or its text blocks
// joined by newlines with its images kept apart. An object carries the
// same in content, plus success.
func (b *builder) readOutput(it item, l *line) *output {
	o := &output{raw: l.Payload, ord: l.ord}
	body := it.Output
	var obj struct {
		Content json.RawMessage `json:"content"`
		Success *bool           `json:"success"`
	}
	if len(body) > 0 && body[0] == '{' && json.Unmarshal(body, &obj) == nil {
		body = obj.Content
		o.failed = obj.Success != nil && !*obj.Success
	}
	if json.Unmarshal(body, &o.text) == nil {
		return o
	}
	var texts []string
	for _, bl := range blocks(body) {
		switch bl.Type {
		case "input_image":
			if p, ok := b.image(bl, l.raw); ok {
				o.images = append(o.images, p)
			}
		default:
			if bl.Text != "" {
				texts = append(texts, bl.Text)
			}
		}
	}
	o.text = strings.Join(texts, "\n")
	return o
}

// toolCall adds a tool_call Part merged with its output (codex.md §3.4).
func (b *builder) toolCall(m *msg, it item) {
	input := json.RawMessage(nil)
	if it.Type == "function_call" && json.Valid([]byte(it.Arguments)) && strings.TrimSpace(it.Arguments) != "" {
		input = json.RawMessage(it.Arguments)
	} else {
		s := it.Input
		if it.Type == "function_call" {
			s = it.Arguments
		}
		input, _ = json.Marshal(s)
	}
	tc := &parser.ToolCallPayload{
		CallID:        it.CallID,
		Name:          it.Name,
		Input:         input,
		Status:        parser.StatusPending,
		ChildSessions: []string{},
	}
	p := &part{kind: parser.KindToolCall, call: tc}
	if o := b.outputs[it.CallID]; o != nil && !o.used {
		o.used = true
		out := o.text
		tc.Output = &out
		tc.Status = parser.StatusOK
		switch {
		case it.Type == "function_call" && o.failed,
			it.Type == "custom_tool_call" && (it.Status == "failed" || it.Status == "incomplete"):
			tc.Status = parser.StatusError
		}
		p.images = o.images
	}
	m.parts = append(m.parts, p)
}

// usage adds a token_usage_record to the latest assistant Message, once per
// response (codex.md §3.3).
func (b *builder) usage(l *line) {
	var r struct {
		ResponseID string `json:"response_id"`
		Usage      *struct {
			Input      int64 `json:"input_tokens"`
			Cached     int64 `json:"cached_input_tokens"`
			CacheWrite int64 `json:"cache_write_input_tokens"`
			Output     int64 `json:"output_tokens"`
			Reasoning  int64 `json:"reasoning_output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(l.Payload, &r) != nil || r.Usage == nil {
		b.warn.Add(parser.WarnMissingField, "token_usage_record.usage", string(l.raw))
		return
	}
	m := b.lastAsst
	if b.cur != nil && len(b.cur.parts) > 0 {
		m = b.cur
	}
	if m == nil {
		return
	}
	if r.ResponseID != "" {
		if b.responses[r.ResponseID] {
			return
		}
		b.responses[r.ResponseID] = true
	}
	if m.Usage == nil {
		m.Usage = &parser.Usage{}
	}
	m.Usage.Add(parser.Usage{
		Input:      r.Usage.Input,
		Output:     r.Usage.Output,
		CacheRead:  r.Usage.Cached,
		CacheWrite: r.Usage.CacheWrite,
		Reasoning:  r.Usage.Reasoning,
	})
}

// messages assigns Part ids and returns the Messages.
func (b *builder) messages() []parser.Message {
	out := make([]parser.Message, 0, len(b.msgs))
	for _, m := range b.msgs {
		pm := m.Message
		add := func(kind string, payload any) {
			pm.Parts = append(pm.Parts, parser.Part{ID: pm.ID + "." + strconv.Itoa(len(pm.Parts)), Kind: kind, Payload: payload})
		}
		for _, p := range m.parts {
			if p.call != nil {
				add(p.kind, *p.call)
				for _, img := range p.images {
					add(parser.KindImage, img)
				}
				continue
			}
			add(p.kind, p.payload)
		}
		out = append(out, pm)
	}
	return out
}

// blocks decodes a block list, keeping each block's raw JSON. It is nil for
// anything else.
func blocks(raw json.RawMessage) []block {
	var rs []json.RawMessage
	if json.Unmarshal(raw, &rs) != nil {
		return nil
	}
	bs := make([]block, 0, len(rs))
	for _, r := range rs {
		var bl block
		if json.Unmarshal(r, &bl) != nil {
			bl = block{Type: "?"}
		}
		bl.raw = r
		bs = append(bs, bl)
	}
	return bs
}
