// Package ohmypi is the Hub parser for the oh-my-pi Source
// (docs/spec/adapters/oh-my-pi.md §2.3, §3).
package ohmypi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// version is the parser_version. Bump it whenever output changes for
// existing data (hub.md §4.5).
const version = 2

const (
	layout     = "jsonl"
	layoutRank = 1
)

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

// keyRe matches a Record key's first two segments, <cwd-dir>/<ts>_<uuid>,
// then either .jsonl or the artifacts directory holding a sub-agent.
var keyRe = regexp.MustCompile(`^[^/]+/\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-\d{3}Z_(` + uuid + `)(\.jsonl$|/)`)

// Parser is the oh-my-pi parser.
type Parser struct{}

// New returns the oh-my-pi parser.
func New() *Parser { return &Parser{} }

// Source implements parser.Parser.
func (*Parser) Source() string { return protocol.SourceOhMyPi }

// Version implements parser.Parser.
func (*Parser) Version() int { return version }

// MapKey implements parser.Parser (oh-my-pi.md §2.3): every Session file is
// its own Session. A sub-agent's native id is its parent's plus /<id>.
func (*Parser) MapKey(key string) (parser.Mapping, bool) {
	id, ok := nativeID(key)
	if !ok {
		return parser.Mapping{}, false
	}
	return parser.Mapping{NativeID: id, Role: parser.RoleMain, Layout: layout, LayoutRank: layoutRank}, true
}

// nativeID derives a Session's native id from its Record key: <uuid> for a
// top-level Session, <uuid>/<rest> for a sub-agent at <ts>_<uuid>/<rest>.jsonl,
// where each nested sub-agent's name extends its parent's with .<sub>.
func nativeID(key string) (string, bool) {
	m := keyRe.FindStringSubmatchIndex(key)
	if m == nil {
		return "", false
	}
	id := key[m[2]:m[3]]
	if key[m[4]:m[5]] == ".jsonl" {
		return id, true
	}
	rest, ok := strings.CutSuffix(key[m[1]:], ".jsonl")
	if !ok || rest == "" {
		return "", false
	}
	segs := strings.Split(rest, "/")
	for i, s := range segs {
		if s == "" || (i > 0 && !strings.HasPrefix(s, segs[i-1]+".")) {
			return "", false
		}
	}
	return id + "/" + rest, true
}

// entry is one line: the union of the fields the parser reads from each
// entry type.
type entry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	ParentID      *string         `json:"parentId"`
	Timestamp     string          `json:"timestamp"`
	Message       *message        `json:"message"`
	Version       int             `json:"version"`
	Title         string          `json:"title"`
	Cwd           string          `json:"cwd"`
	Task          string          `json:"task"`
	Agent         string          `json:"agent"`
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinkingLevel"`
	Summary       string          `json:"summary"`
	CustomType    string          `json:"customType"`
	Display       bool            `json:"display"`
	Details       json.RawMessage `json:"details"`

	raw  []byte
	ts   int64
	line int
}

// message is a message entry's message.
type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	Provider   string          `json:"provider"`
	Usage      *usage          `json:"usage"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Details    json.RawMessage `json:"details"`
}

type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoningTokens"`
}

// block is one content block.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Data      string          `json:"data"`
	MimeType  string          `json:"mimeType"`

	raw json.RawMessage
}

// Parse implements parser.Parser.
func (*Parser) Parse(in parser.Input) (parser.Result, error) {
	var (
		res  parser.Result
		warn parser.Warnings
	)
	var slot, header, init *entry
	var body []*entry
	n := 0
	for raw := range bytes.Lines(in.Main) {
		n++
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		e := &entry{raw: raw, line: n}
		if err := json.Unmarshal(raw, e); err != nil {
			warn.Add(parser.WarnBadLine, "", string(raw))
			continue
		}
		e.ts = parseTime(e.Timestamp)
		if e.ts > res.Session.LastActivityAt {
			res.Session.LastActivityAt = e.ts
		}
		switch {
		case e.Type == "title" && slot == nil && header == nil && len(body) == 0:
			slot = e
		case e.Type == "session" && header == nil:
			header = e
		default:
			if e.Type == "session_init" && init == nil {
				init = e
			}
			body = append(body, e)
		}
	}

	s := &res.Session
	if header == nil {
		warn.Add(parser.WarnMissingField, "session", "")
		header = &entry{}
	} else {
		s.SourceVersion = "schema-" + strconv.Itoa(header.Version)
		if header.Cwd == "" {
			warn.Add(parser.WarnMissingField, "cwd", string(header.raw))
		}
	}
	s.StartedAt, s.Cwd = header.ts, header.Cwd
	if s.StartedAt == 0 && len(body) > 0 {
		s.StartedAt = body[0].ts
	}
	if i := strings.LastIndex(in.NativeID, "/"); i >= 0 {
		s.ParentNativeID = in.NativeID[:i]
	}
	switch {
	case slot != nil && slot.Title != "":
		s.Title = slot.Title
	case header.Title != "":
		s.Title = header.Title
	case s.ParentNativeID != "" && init != nil && init.Agent != "":
		s.Title = init.Agent + ": " + strings.TrimSuffix(path.Base(in.MainKey), ".jsonl")
	}

	var entries []*entry
	if isV1(header.Version, body) {
		entries = body
	} else {
		entries = latestPath(body, &warn)
	}
	b := &builder{warn: &warn, native: in.NativeID, results: map[string]*entry{}, calls: map[string]bool{}}
	b.build(entries)
	res.Messages, res.Images = b.messages(), b.images
	res.Warnings = warn.List()
	return res, nil
}

// isV1 reports whether a file has no entry tree: its header says version
// 1, or has no version and no entry has an id (oh-my-pi.md §3.2).
func isV1(version int, body []*entry) bool {
	if version != 0 {
		return version == 1
	}
	for _, e := range body {
		if e.ID != "" {
			return false
		}
	}
	return true
}

// latestPath is the path from the root to the latest leaf, the last entry
// with an id (oh-my-pi.md §3.2). A parentId naming no entry stops the walk.
func latestPath(body []*entry, warn *parser.Warnings) []*entry {
	byID := map[string]*entry{}
	var leaf *entry
	for _, e := range body {
		if e.ID != "" {
			byID[e.ID] = e
			leaf = e
		}
	}
	var out []*entry
	seen := map[*entry]bool{}
	for e := leaf; e != nil && !seen[e]; {
		seen[e] = true
		out = append(out, e)
		if e.ParentID == nil || *e.ParentID == "" {
			break
		}
		p, ok := byID[*e.ParentID]
		if !ok {
			warn.Add(parser.WarnOrphan, e.Type, string(e.raw))
		}
		e = p
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// parseTime is an RFC 3339 time in Unix milliseconds, 0 when it doesn't parse.
func parseTime(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// builder turns the entries on the path into Messages (oh-my-pi.md §3.1–§3.4).
type builder struct {
	warn    *parser.Warnings
	native  string
	msgs    []*parser.Message
	images  []parser.Image
	results map[string]*entry // toolResult entries by toolCallId
	calls   map[string]bool   // toolCall ids on the path
}

func (b *builder) build(entries []*entry) {
	for _, e := range entries {
		if e.Type != "message" || e.Message == nil {
			continue
		}
		switch e.Message.Role {
		case "toolResult":
			if _, dup := b.results[e.Message.ToolCallID]; !dup {
				b.results[e.Message.ToolCallID] = e
			}
		case "assistant":
			for _, bl := range blocks(e.Message.Content) {
				if bl.Type == "toolCall" {
					b.calls[bl.ID] = true
				}
			}
		}
	}

	for i, e := range entries {
		switch e.Type {
		case "message":
			b.message(e)
		case "session_init":
			if next := nextMessage(entries[i+1:]); next != nil && next.Message.Role == "user" && contentText(next.Message.Content) == e.Task {
				// omp 18 repeats the task as a user message, which stands in for it.
				continue
			}
			if e.Task != "" {
				m := b.newMsg(e, parser.MessageUser)
				addPart(m, parser.KindText, parser.TextPayload{Text: e.Task})
				b.msgs = append(b.msgs, m)
			}
		case "model_change":
			b.marker(e, parser.MessageAssistant, parser.MarkerModelChange, e.Model)
		case "thinking_level_change":
			b.marker(e, parser.MessageAssistant, parser.MarkerThinkingLevel, e.ThinkingLevel)
		case "compaction":
			text := e.Summary
			if strings.TrimSpace(text) == "" {
				text = parser.CompactionText
			}
			b.marker(e, parser.MessageAssistant, parser.MarkerCompaction, text)
		case "branch_summary":
			text := "Branch discarded"
			if strings.TrimSpace(e.Summary) != "" {
				text = "Branch summary: " + e.Summary
			}
			b.marker(e, parser.MessageAssistant, parser.MarkerCompaction, text)
		case "reset_boundary":
			b.marker(e, parser.MessageUser, parser.MarkerSlashCommand, "/clear")
		case "custom_message":
			if e.Display {
				text := e.CustomType
				var d struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(e.Details, &d) == nil && d.Name != "" {
					text += ": " + d.Name
				}
				b.marker(e, parser.MessageUser, parser.MarkerSlashCommand, text)
			}
		case "title", "session", "title_change", "credential_pin", "model_usage", "custom",
			"service_tier_change", "ttsr_injection", "mode_change", "label":
			// Raw only.
		default:
			b.warn.Add(parser.WarnUnknownType, e.Type, string(e.raw))
			m := b.newMsg(e, parser.MessageAssistant)
			addPart(m, parser.KindUnknown, parser.NewUnknown(e.Type, e.raw))
			b.msgs = append(b.msgs, m)
		}
	}
}

// nextMessage is the first message entry in entries.
func nextMessage(entries []*entry) *entry {
	for _, e := range entries {
		if e.Type == "message" && e.Message != nil {
			return e
		}
	}
	return nil
}

// msgID is the id of the Message starting at e (oh-my-pi.md §3.7): the
// entry id, or for a v1 entry a hash of the native id and line position.
func (b *builder) msgID(e *entry) string {
	if e.ID != "" {
		return parser.SafeID(e.ID)
	}
	sum := sha256.Sum256([]byte(b.native + "\x00" + strconv.Itoa(e.line)))
	return hex.EncodeToString(sum[:8])
}

func (b *builder) newMsg(e *entry, role string) *parser.Message {
	return &parser.Message{ID: b.msgID(e), Role: role, Timestamp: e.ts}
}

func addPart(m *parser.Message, kind string, payload any) {
	m.Parts = append(m.Parts, parser.Part{ID: m.ID + "." + strconv.Itoa(len(m.Parts)), Kind: kind, Payload: payload})
}

func (b *builder) marker(e *entry, role, marker, text string) {
	m := b.newMsg(e, role)
	addPart(m, parser.KindMarker, parser.MarkerPayload{Marker: marker, Text: text})
	b.msgs = append(b.msgs, m)
}

// message maps one message entry by role (oh-my-pi.md §3.3).
func (b *builder) message(e *entry) {
	msg := e.Message
	switch msg.Role {
	case "user":
		m := b.newMsg(e, parser.MessageUser)
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			addPart(m, parser.KindText, parser.TextPayload{Text: s})
		} else {
			b.content(m, msg.Content, e.raw)
		}
		b.add(m)
	case "assistant":
		m := b.newMsg(e, parser.MessageAssistant)
		m.Model, m.Provider = msg.Model, msg.Provider
		if u := msg.Usage; u != nil {
			m.Usage = &parser.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Reasoning: u.Reasoning}
		}
		b.content(m, msg.Content, e.raw)
		b.add(m)
	case "toolResult":
		if !b.calls[msg.ToolCallID] {
			b.warn.Add(parser.WarnOrphan, "toolResult", string(e.raw))
		}
	case "developer", "custom", "hookMessage":
		// Injected reminders and extension messages: Raw only.
	default:
		m := b.newMsg(e, parser.MessageAssistant)
		b.unknown(m, "message:"+msg.Role, e.raw)
		b.add(m)
	}
}

// add keeps a Message that has Parts.
func (b *builder) add(m *parser.Message) {
	if len(m.Parts) > 0 {
		b.msgs = append(b.msgs, m)
	}
}

func (b *builder) unknown(m *parser.Message, sourceType string, raw []byte) {
	b.warn.Add(parser.WarnUnknownType, sourceType, string(raw))
	addPart(m, parser.KindUnknown, parser.NewUnknown(sourceType, raw))
}

// content adds a message's content blocks as Parts.
func (b *builder) content(m *parser.Message, content json.RawMessage, raw []byte) {
	for _, bl := range blocks(content) {
		switch bl.Type {
		case "text":
			addPart(m, parser.KindText, parser.TextPayload{Text: bl.Text})
		case "thinking":
			if strings.TrimSpace(bl.Thinking) != "" {
				addPart(m, parser.KindThinking, parser.ThinkingPayload{Text: bl.Thinking})
			}
		case "toolCall":
			b.toolCall(m, bl)
		case "image":
			b.image(m, bl, raw)
		default:
			b.unknown(m, bl.Type, bl.raw)
		}
	}
}

// blobPrefix marks an image held in omp's shared blob store.
const blobPrefix = "blob:sha256:"

// image adds an inline image, or an attachment label for a blob reference.
func (b *builder) image(m *parser.Message, bl block, raw []byte) {
	if hash, ok := strings.CutPrefix(bl.Data, blobPrefix); ok {
		addPart(m, parser.KindAttachment, parser.AttachmentPayload{Label: "image (blob " + parser.CutBytes(hash, 12) + ")"})
		return
	}
	bs, err := base64.StdEncoding.DecodeString(bl.Data)
	if err != nil || len(bs) == 0 {
		b.warn.Add(parser.WarnMissingField, "image.data", string(raw))
		return
	}
	p, img := parser.NewImage(bl.MimeType, bs)
	b.images = append(b.images, img)
	addPart(m, parser.KindImage, p)
}

// toolCall adds a tool_call Part merged with its result, then the result's
// images (oh-my-pi.md §3.4).
func (b *builder) toolCall(m *parser.Message, bl block) {
	input := bl.Arguments
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	tc := parser.ToolCallPayload{
		CallID:        bl.ID,
		Name:          bl.Name,
		Input:         input,
		Status:        parser.StatusPending,
		ChildSessions: []string{},
	}
	r := b.results[bl.ID]
	var images []block
	if r != nil {
		var texts []string
		for _, rb := range blocks(r.Message.Content) {
			switch rb.Type {
			case "text":
				texts = append(texts, rb.Text)
			case "image":
				images = append(images, rb)
			}
		}
		out := strings.Join(texts, "\n")
		tc.Output = &out
		tc.Status = parser.StatusOK
		if r.Message.IsError {
			tc.Status = parser.StatusError
		}
		if bl.Name == "task" {
			tc.ChildSessions = b.children(r.Message.Details)
		}
	}
	addPart(m, parser.KindToolCall, tc)
	for _, ib := range images {
		b.image(m, ib, r.raw)
	}
}

// children are the native ids of the sub-agents a task result names: its
// results, else its progress while they run.
func (b *builder) children(details json.RawMessage) []string {
	var d struct {
		Results  []struct{ ID string } `json:"results"`
		Progress []struct{ ID string } `json:"progress"`
	}
	json.Unmarshal(details, &d)
	list := d.Results
	if len(list) == 0 {
		list = d.Progress
	}
	out := []string{}
	for _, c := range list {
		if c.ID != "" {
			out = append(out, b.native+"/"+c.ID)
		}
	}
	return out
}

func (b *builder) messages() []parser.Message {
	out := make([]parser.Message, 0, len(b.msgs))
	for _, m := range b.msgs {
		out = append(out, *m)
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

// contentText is a message content's text: the string itself, or its text
// blocks joined.
func contentText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var texts []string
	for _, bl := range blocks(content) {
		if bl.Type == "text" {
			texts = append(texts, bl.Text)
		}
	}
	return strings.Join(texts, "")
}
