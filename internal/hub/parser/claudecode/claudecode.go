// Package claudecode is the Hub parser for the claude-code Source
// (docs/spec/adapters/claude-code.md §2.3, §3).
package claudecode

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"html"
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
const version = 12

const (
	layout     = "jsonl"
	layoutRank = 1
)

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	childRe = regexp.MustCompile(`^agent-([^/.]+)\.(jsonl|meta\.json)$`)
)

// Parser is the claude-code parser.
type Parser struct{}

// New returns the claude-code parser.
func New() *Parser { return &Parser{} }

// Source implements parser.Parser.
func (*Parser) Source() string { return protocol.SourceClaudeCode }

// Version implements parser.Parser.
func (*Parser) Version() int { return version }

// MapKey implements parser.Parser (claude-code.md §2.3).
func (*Parser) MapKey(key string) (parser.Mapping, bool) {
	segs := strings.Split(key, "/")
	if len(segs) < 2 || segs[0] == "" {
		return parser.Mapping{}, false
	}
	m := func(native, role string) (parser.Mapping, bool) {
		return parser.Mapping{NativeID: native, Role: role, Layout: layout, LayoutRank: layoutRank}, true
	}
	name := segs[1]
	if len(segs) == 2 {
		session, rest, _ := strings.Cut(name, ".")
		if !uuidRe.MatchString(session) {
			return parser.Mapping{}, false
		}
		switch {
		case rest == "jsonl":
			return m(session, parser.RoleMain)
		case strings.HasPrefix(rest, "orphaned-") && strings.HasSuffix(rest, ".jsonl"),
			strings.HasPrefix(rest, "jsonl.superseded-"):
			return m(session, parser.RoleAttachment)
		}
		return parser.Mapping{}, false
	}
	if !uuidRe.MatchString(name) {
		return parser.Mapping{}, false
	}
	if len(segs) == 4 && segs[2] == "subagents" {
		if a := childRe.FindStringSubmatch(segs[3]); a != nil {
			role := parser.RoleMain
			if a[2] == "meta.json" {
				role = parser.RoleAttachment
			}
			return m(childNativeID(name, a[1]), role)
		}
	}
	return m(name, parser.RoleAttachment)
}

// line is the subset of a JSONL line the parser reads.
type line struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	UUID              string          `json:"uuid"`
	ParentUUID        *string         `json:"parentUuid"`
	LogicalParentUUID *string         `json:"logicalParentUuid"`
	IsSidechain       bool            `json:"isSidechain"`
	IsMeta            bool            `json:"isMeta"`
	IsCompactSummary  bool            `json:"isCompactSummary"`
	Content           json.RawMessage `json:"content"` // system lines
	Prompt            string          `json:"prompt"`  // scheduled_task_fire
	Attachment        json.RawMessage `json:"attachment"`
	Timestamp         string          `json:"timestamp"`
	Cwd               string          `json:"cwd"`
	GitBranch         string          `json:"gitBranch"`
	Version           string          `json:"version"`
	Message           json.RawMessage `json:"message"`
	CustomTitle       string          `json:"customTitle"`
	AITitle           string          `json:"aiTitle"`
	Summary           string          `json:"summary"`
	ToolUseResult     json.RawMessage `json:"toolUseResult"`
	Origin            struct {
		Kind string `json:"kind"`
	} `json:"origin"`

	raw []byte
	ts  int64
	// unknown is the Source type of a line the parser doesn't know, "" for
	// a known one.
	unknown string
}

// rawOnly are the line types kept in the Raw record only, beyond user,
// assistant, system and attachment (claude-code.md §3.1).
var rawOnly = map[string]bool{
	"summary": true, "custom-title": true, "ai-title": true,
	"file-history-snapshot": true, "file-history-delta": true, "last-prompt": true, "mode": true,
	"permission-mode": true, "queue-operation": true, "progress": true, "atis-latch": true,
	"bridge-session": true, "cost-state": true, "agent-name": true, "pr-link": true, "frame-link": true,
	"artifact-autoreact-ledger": true, "artifact-comment-monitor": true,
}

// rawOnlySystem are the system subtypes kept in the Raw record only.
var rawOnlySystem = map[string]bool{
	"turn_duration": true, "local_command": true, "away_summary": true,
	"stop_hook_summary": true, "informational": true, "api_error": true, "bridge_status": true,
}

const (
	compactBoundary   = "compact_boundary"
	scheduledTaskFire = "scheduled_task_fire"
)

// unknownType returns the Source type of a line the parser doesn't know
// (claude-code.md §3.1): its type, or system:<subtype> for an unknown system
// subtype. It is "" for a known line.
func unknownType(l *line) string {
	switch {
	case l.Type == "user", l.Type == "assistant", l.Type == "attachment", rawOnly[l.Type]:
		return ""
	case l.Type == "system":
		if l.Subtype == compactBoundary || l.Subtype == scheduledTaskFire || rawOnlySystem[l.Subtype] {
			return ""
		}
		return "system:" + l.Subtype
	}
	return l.Type
}

type apiMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		OutputTokensDetails      struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// block is one content block of a message: the union of the fields the
// parser reads from each block type.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result
	IsError   bool            `json:"is_error"`    // tool_result
	ToolName  string          `json:"tool_name"`   // tool_reference
	Thinking  string          `json:"thinking"`    // thinking
	Title     string          `json:"title"`       // document
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"` // image, document

	raw json.RawMessage
}

// Parse implements parser.Parser.
func (*Parser) Parse(in parser.Input) (parser.Result, error) {
	var (
		res   parser.Result
		warn  parser.Warnings
		lines []*line
	)
	isChild := strings.Contains(in.NativeID, "/")
	if isChild {
		res.Session.ParentNativeID, _, _ = strings.Cut(in.NativeID, "/")
	}

	var customTitle, aiTitle, summary string
	for raw := range bytes.Lines(in.Main) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		l := &line{raw: raw}
		if err := json.Unmarshal(raw, l); err != nil {
			warn.Add(parser.WarnBadLine, "", string(raw))
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			l.ts = t.UnixMilli()
			if res.Session.StartedAt == 0 || l.ts < res.Session.StartedAt {
				res.Session.StartedAt = l.ts
			}
			if l.ts > res.Session.LastActivityAt {
				res.Session.LastActivityAt = l.ts
			}
		}
		if l.unknown = unknownType(l); l.unknown != "" {
			warn.Add(parser.WarnUnknownType, l.unknown, string(raw))
		}
		if res.Session.Cwd == "" {
			res.Session.Cwd = l.Cwd
		}
		if l.GitBranch != "" {
			res.Session.GitBranch = l.GitBranch
		}
		if l.Version != "" {
			res.Session.SourceVersion = l.Version
		}
		switch l.Type {
		case "custom-title":
			customTitle = l.CustomTitle
		case "ai-title":
			aiTitle = l.AITitle
		case "summary":
			summary = l.Summary
		}
		lines = append(lines, l)
	}
	if res.Session.Cwd == "" {
		warn.Add(parser.WarnMissingField, "cwd", "")
	}
	if isChild {
		var ok bool
		res.Session.Title, res.Session.SpawningCallID, ok = childMeta(in)
		if !ok {
			warn.Add(parser.WarnMissingField, "meta.json", "")
		}
	} else {
		res.Session.Title = firstNonEmpty(customTitle, titleFromFile(in), aiTitle, summary)
	}

	res.Messages, res.Images = buildMessages(in, transcriptPath(lines, isChild, &warn), &warn)
	res.Warnings = warn.List()
	return res, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// titleFromFile reads customTitle from the Session's custom-title.json attachment.
func titleFromFile(in parser.Input) string {
	for key, b := range in.Attachments {
		if strings.HasSuffix(key, "/custom-title.json") {
			var v struct {
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(b, &v) == nil {
				return v.CustomTitle
			}
		}
	}
	return ""
}

// childMeta reads a Child Session's .meta.json attachment: its title (the
// description) and the spawning call (claude-code.md §3.5, §3.6). ok is false
// when the attachment is missing or doesn't decode.
func childMeta(in parser.Input) (title, spawningCallID string, ok bool) {
	for key, b := range in.Attachments {
		if strings.HasSuffix(key, ".meta.json") {
			var v struct {
				Description string `json:"description"`
				ToolUseID   string `json:"toolUseId"`
			}
			if json.Unmarshal(b, &v) == nil {
				return v.Description, v.ToolUseID, true
			}
		}
	}
	return "", "", false
}

// transcriptPath returns the lines from the root to the latest leaf
// (claude-code.md §3.2).
func transcriptPath(lines []*line, isChild bool, warn *parser.Warnings) []*line {
	byUUID := map[string]*line{}
	var leaf *line
	for _, l := range lines {
		if l.UUID == "" {
			continue
		}
		byUUID[l.UUID] = l
		if isChild || !l.IsSidechain {
			leaf = l
		}
	}
	var path []*line
	seen := map[string]bool{}
	for l := leaf; l != nil && !seen[l.UUID]; {
		seen[l.UUID] = true
		path = append(path, l)
		next := l.ParentUUID
		if next == nil {
			next = l.LogicalParentUUID
		}
		if next == nil {
			break
		}
		p, ok := byUUID[*next]
		if !ok {
			warn.Add(parser.WarnOrphan, "parentUuid", string(l.raw))
			break
		}
		l = p
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return withSideResults(lines, path, isChild)
}

// withSideResults splices in the tool results the path leaves out. Claude
// Code chains parallel calls one onto the next, so each call's result but the
// last hangs off the path as a side branch. A side line holding only
// tool_results, whose parent is on the path, goes right after that parent when
// it answers a call no path line does (claude-code.md §3.2 step 6).
func withSideResults(lines, path []*line, isChild bool) []*line {
	onPath := map[*line]bool{}
	answered := map[string]bool{}
	for _, l := range path {
		onPath[l] = true
		for _, bl := range userBlocks(l) {
			if isToolResult(bl) {
				answered[bl.ToolUseID] = true
			}
		}
	}
	side := map[string][]*line{} // by parent uuid, in file order
	for _, l := range lines {
		if onPath[l] || l.Type != "user" || l.ParentUUID == nil || (!isChild && l.IsSidechain) {
			continue
		}
		blocks := userBlocks(l)
		if len(blocks) == 0 || !slices.ContainsFunc(blocks, isToolResult) ||
			slices.ContainsFunc(blocks, func(bl block) bool { return !isToolResult(bl) }) {
			continue
		}
		side[*l.ParentUUID] = append(side[*l.ParentUUID], l)
	}
	if len(side) == 0 {
		return path
	}
	var out []*line
	for _, l := range path {
		out = append(out, l)
		for _, r := range side[l.UUID] {
			blocks := userBlocks(r)
			if !slices.ContainsFunc(blocks, func(bl block) bool { return !answered[bl.ToolUseID] }) {
				continue
			}
			for _, bl := range blocks {
				answered[bl.ToolUseID] = true
			}
			out = append(out, r)
		}
	}
	return out
}

// builder turns path lines into Messages (claude-code.md §3.3, §3.4).
type builder struct {
	in      parser.Input
	warn    *parser.Warnings
	results map[string]*result // tool_result blocks on the path, by tool_use_id
	images  []parser.Image
}

// result is one tool_result block and where it sits on the path.
type result struct {
	block
	pos     int
	used    bool
	raw     []byte
	agentID string // toolUseResult.agentId of its line: the Child Session it ran
}

// buildMessages turns path lines into Messages (claude-code.md §3.3, §3.4).
// Assistant lines sharing a message.id form one Message. A tool-result-only
// user line doesn't break the group: parallel tool calls interleave each
// tool_use line with its result. Any other user line does.
func buildMessages(in parser.Input, path []*line, warn *parser.Warnings) ([]parser.Message, []parser.Image) {
	b := &builder{in: in, warn: warn, results: map[string]*result{}}
	for i, l := range path {
		if l.Type != "user" {
			continue
		}
		blocks := userBlocks(l)
		// The line's toolUseResult belongs to its only tool_result; with
		// several, none can claim its agentId.
		lineAgent := ""
		if n := slices.IndexFunc(blocks, isToolResult); n >= 0 && !slices.ContainsFunc(blocks[n+1:], isToolResult) {
			lineAgent = agentID(l)
		}
		for _, bl := range blocks {
			if isToolResult(bl) && bl.ToolUseID != "" {
				if _, dup := b.results[bl.ToolUseID]; !dup {
					b.results[bl.ToolUseID] = &result{block: bl, pos: i, raw: l.raw, agentID: lineAgent}
				}
			}
		}
	}

	var (
		msgs     []parser.Message
		cur      *parser.Message
		curAPIID string
	)
	flush := func() {
		if cur != nil {
			msgs = append(msgs, *cur)
		}
		cur, curAPIID = nil, ""
	}
	// marker appends a Message holding one marker Part.
	marker := func(l *line, role string, mp parser.MarkerPayload) {
		m := parser.Message{ID: parser.SafeID(l.UUID), Role: role, Timestamp: l.ts}
		addPart(&m, parser.KindMarker, mp)
		msgs = append(msgs, m)
	}
	for i := 0; i < len(path); i++ {
		l := path[i]
		if l.unknown != "" {
			flush()
			m := parser.Message{ID: parser.SafeID(l.UUID), Role: parser.MessageAssistant, Timestamp: l.ts}
			addPart(&m, parser.KindUnknown, parser.NewUnknown(l.unknown, l.raw))
			msgs = append(msgs, m)
			continue
		}
		switch l.Type {
		case "system":
			if l.Subtype == scheduledTaskFire {
				// The isMeta line after it repeats the prompt and stays Raw only.
				flush()
				var text string
				json.Unmarshal(l.Content, &text)
				marker(l, parser.MessageUser, parser.MarkerPayload{Marker: parser.MarkerScheduledTask, Text: text, Output: l.Prompt})
				continue
			}
			if l.Subtype != compactBoundary {
				continue
			}
			flush()
			var text string
			if json.Unmarshal(l.Content, &text) != nil || text == "" {
				text = parser.CompactionText
			}
			marker(l, parser.MessageAssistant, parser.MarkerPayload{Marker: parser.MarkerCompaction, Text: text})
		case "attachment":
			// Injected context is Raw only, except a queued prompt.
			var a struct {
				Type        string          `json:"type"`
				IsMeta      bool            `json:"isMeta"`
				Prompt      json.RawMessage `json:"prompt"`
				CommandMode string          `json:"commandMode"`
			}
			if json.Unmarshal(l.Attachment, &a) != nil || a.Type != "queued_command" || a.IsMeta {
				continue
			}
			flush()
			var prompt string
			isText := json.Unmarshal(a.Prompt, &prompt) == nil
			if isText && (a.CommandMode == taskMode || strings.HasPrefix(prompt, taskOpen)) {
				if mp, ok := taskNotification(prompt); ok {
					marker(l, parser.MessageUser, mp)
					continue
				}
				warn.Add(parser.WarnMissingField, taskMode, string(l.raw))
			}
			m := parser.Message{ID: parser.SafeID(l.UUID), Role: parser.MessageUser, Timestamp: l.ts}
			if isText {
				addText(&m, prompt)
			} else {
				// A queued prompt with images is a block list.
				for _, bl := range contentBlocks(a.Prompt) {
					switch bl.Type {
					case "text":
						addText(&m, bl.Text)
					case "image":
						b.addImage(&m, bl, l.raw)
					default:
						b.addUnknown(&m, bl)
					}
				}
			}
			if len(m.Parts) == 0 {
				warn.Add(parser.WarnMissingField, "attachment.prompt", string(l.raw))
				continue
			}
			msgs = append(msgs, m)
		case "user":
			kind, blocks, text := classifyUser(l, warn)
			if kind == userToolResults {
				continue
			}
			flush()
			switch kind {
			case userSkip:
				continue
			case userCompaction:
				marker(l, parser.MessageUser, parser.MarkerPayload{Marker: parser.MarkerCompaction, Text: text})
				continue
			case userCommand:
				marker(l, parser.MessageUser, parser.MarkerPayload{Marker: parser.MarkerSlashCommand, Text: text})
				continue
			case userShell:
				// Its output is the next path line, when that is one.
				var out string
				if i+1 < len(path) {
					if o, ok := shellOutput(path[i+1]); ok {
						out = o
						i++
					}
				}
				marker(l, parser.MessageUser, parser.MarkerPayload{Marker: parser.MarkerShellCommand, Text: text, Output: out})
				continue
			case userTask:
				if mp, ok := taskNotification(text); ok {
					marker(l, parser.MessageUser, mp)
					continue
				}
				// Malformed: shown as the text it is.
				warn.Add(parser.WarnMissingField, taskMode, string(l.raw))
				blocks = []block{{Type: "text", Text: text}}
			}
			m := parser.Message{ID: parser.SafeID(l.UUID), Role: parser.MessageUser, Timestamp: l.ts}
			for _, bl := range blocks {
				switch bl.Type {
				case "text":
					addText(&m, bl.Text)
				case "image":
					b.addImage(&m, bl, l.raw)
				case "document":
					label := bl.Title
					if label == "" && bl.Source != nil {
						label = bl.Source.MediaType
					}
					addPart(&m, parser.KindAttachment, parser.AttachmentPayload{Label: label})
				case "tool_result":
					// Merged into its call.
				default:
					b.addUnknown(&m, bl)
				}
			}
			msgs = append(msgs, m)
		case "assistant":
			var am apiMessage
			if json.Unmarshal(l.Message, &am) != nil {
				warn.Add(parser.WarnMissingField, "message", string(l.raw))
				continue
			}
			if cur == nil || am.ID == "" || am.ID != curAPIID {
				flush()
				cur = &parser.Message{ID: parser.SafeID(l.UUID), Role: parser.MessageAssistant, Timestamp: l.ts, Model: am.Model, Provider: "anthropic"}
				curAPIID = am.ID
			}
			if am.Model != "" {
				cur.Model = am.Model
			}
			// Usage comes from the group's last line.
			cur.Usage = nil
			if u := am.Usage; u != nil {
				cur.Usage = &parser.Usage{
					Input:      u.InputTokens,
					Output:     u.OutputTokens,
					CacheRead:  u.CacheReadInputTokens,
					CacheWrite: u.CacheCreationInputTokens,
					Reasoning:  u.OutputTokensDetails.ThinkingTokens,
				}
			}
			for _, bl := range contentBlocks(am.Content) {
				switch bl.Type {
				case "text":
					addText(cur, bl.Text)
				case "thinking":
					// Claude Code often keeps only the signature; an empty
					// thinking block has nothing to show.
					if bl.Thinking != "" {
						addPart(cur, parser.KindThinking, parser.ThinkingPayload{Text: bl.Thinking})
					}
				case "redacted_thinking":
					// Raw only.
				case "tool_use":
					b.addToolCall(cur, bl, i)
				case "image":
					b.addImage(cur, bl, l.raw)
				default:
					b.addUnknown(cur, bl)
				}
			}
		}
	}
	flush()
	linkTaskNotifications(msgs)

	// Results whose call isn't on the path, in path order for a stable excerpt.
	var orphans []*result
	for _, r := range b.results {
		if !r.used {
			orphans = append(orphans, r)
		}
	}
	slices.SortFunc(orphans, func(a, b *result) int { return cmp.Or(a.pos-b.pos, strings.Compare(a.ToolUseID, b.ToolUseID)) })
	for _, r := range orphans {
		warn.Add(parser.WarnOrphan, "tool_result", string(r.raw))
	}
	return msgs, b.images
}

const (
	// taskMode is a task notification's origin.kind and commandMode, and
	// the source type of its Parse warning.
	taskMode  = "task-notification"
	taskOpen  = "<task-notification>"
	taskClose = "</task-notification>"
)

// taskNotification reads a <task-notification> block (claude-code.md §3.3).
// ok is false when it is malformed: unclosed, or without a summary.
//
// A <result> or <event> is free text that can quote any of the block's own
// tags, so each runs from its first opening tag to its last closing one, the
// short tags are read only before them, and <usage> only after them.
func taskNotification(s string) (mp parser.MarkerPayload, ok bool) {
	body, found := strings.CutPrefix(strings.TrimSpace(s), taskOpen)
	if !found {
		return mp, false
	}
	end := strings.LastIndex(body, taskClose)
	if end < 0 {
		return mp, false
	}
	body = body[:end]
	head, tail := body, body
	for _, tag := range []string{"result", "event"} {
		if i := strings.Index(body, "<"+tag+">"); i >= 0 && i < len(head) {
			head = body[:i]
		}
		if i := strings.LastIndex(body, "</"+tag+">"); i >= 0 && len(body)-i < len(tail) {
			tail = body[i:]
		}
	}
	summary := strings.TrimSpace(tagBody(head, "summary"))
	if summary == "" {
		return mp, false
	}
	usage := tagBody(tail, "usage")
	num := func(tag string) int64 {
		n, _ := strconv.ParseInt(strings.TrimSpace(tagBody(usage, tag)), 10, 64)
		return n
	}
	return parser.MarkerPayload{
		Marker: parser.MarkerTaskNotification,
		Text:   summary,
		Output: strings.TrimSpace(outerTagBody(body, "event")),
		Task: &parser.TaskPayload{
			Status:     strings.TrimSpace(tagBody(head, "status")),
			ToolUseID:  strings.TrimSpace(tagBody(head, "tool-use-id")),
			Result:     strings.TrimSpace(outerTagBody(body, "result")),
			Tokens:     num("subagent_tokens"),
			ToolUses:   num("tool_uses"),
			DurationMS: num("duration_ms"),
		},
	}, true
}

// tagBody is the text between the first <tag> and the </tag> after it, or
// "" when either is missing.
func tagBody(s, tag string) string {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return ""
	}
	body, _, ok := strings.Cut(rest, "</"+tag+">")
	if !ok {
		return ""
	}
	return body
}

// outerTagBody is the text between the first <tag> and the last </tag>, or ""
// when either is missing.
func outerTagBody(s, tag string) string {
	_, rest, ok := strings.Cut(s, "<"+tag+">")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "</"+tag+">")
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// linkTaskNotifications links each task_notification marker and the
// tool_call Part that started its task, both ways, when that call is in the
// Session.
func linkTaskNotifications(msgs []parser.Message) {
	calls := map[string]*parser.Part{}
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			if tc, ok := p.Payload.(parser.ToolCallPayload); ok && calls[tc.CallID] == nil {
				calls[tc.CallID] = p
			}
		}
	}
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			mp, ok := p.Payload.(parser.MarkerPayload)
			if !ok || mp.Task == nil || mp.Task.ToolUseID == "" {
				continue
			}
			call := calls[mp.Task.ToolUseID]
			if call == nil {
				continue
			}
			task := *mp.Task
			task.CallPart = call.ID
			mp.Task = &task
			p.Payload = mp
			tc := call.Payload.(parser.ToolCallPayload)
			tc.Notifications = append(tc.Notifications, p.ID)
			call.Payload = tc
		}
	}
}

func addPart(m *parser.Message, kind string, payload any) {
	m.Parts = append(m.Parts, parser.Part{
		ID:      m.ID + "." + strconv.Itoa(len(m.Parts)),
		Kind:    kind,
		Payload: payload,
	})
}

func addText(m *parser.Message, text string) {
	addPart(m, parser.KindText, parser.TextPayload{Text: text})
}

// addUnknown adds an unknown Part for a block type the parser doesn't know,
// with an unknown_type warning.
func (b *builder) addUnknown(m *parser.Message, bl block) {
	b.warn.Add(parser.WarnUnknownType, bl.Type, string(bl.raw))
	addPart(m, parser.KindUnknown, parser.NewUnknown(bl.Type, bl.raw))
}

// addImage adds an image Part for a base64 image block. Other sources carry
// no bytes: they are skipped with an unknown_type warning.
func (b *builder) addImage(m *parser.Message, bl block, raw []byte) {
	if bl.Source == nil || bl.Source.Type != "base64" {
		b.warn.Add(parser.WarnUnknownType, "image", string(raw))
		return
	}
	data, err := base64.StdEncoding.DecodeString(bl.Source.Data)
	if err != nil {
		b.warn.Add(parser.WarnMissingField, "image.source.data", string(raw))
		return
	}
	payload, img := parser.NewImage(bl.Source.MediaType, data)
	b.images = append(b.images, img)
	addPart(m, parser.KindImage, payload)
}

// addToolCall adds a tool_call Part merged with its result from a later path
// line, then any images the result holds (claude-code.md §3.4).
func (b *builder) addToolCall(m *parser.Message, bl block, pos int) {
	input := bl.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	p := parser.ToolCallPayload{
		CallID:        bl.ID,
		Name:          bl.Name,
		Input:         input,
		Status:        parser.StatusPending,
		ChildSessions: []string{},
	}
	if bl.Name == "Edit" {
		var e struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if json.Unmarshal(bl.Input, &e) == nil {
			p.Diff = &parser.Diff{Path: e.FilePath, Old: e.OldString, New: e.NewString}
		}
	}
	r := b.results[bl.ID]
	if r == nil || r.used || r.pos <= pos {
		addPart(m, parser.KindToolCall, p)
		return
	}
	r.used = true
	p.Status = parser.StatusOK
	if r.IsError {
		p.Status = parser.StatusError
	}
	if (bl.Name == "Agent" || bl.Name == "Task") && r.agentID != "" {
		// Nested Child Sessions are filed flat under the top-level Session.
		top, _, _ := strings.Cut(b.in.NativeID, "/")
		p.ChildSessions = []string{childNativeID(top, r.agentID)}
	}
	out, images := resultContent(r.Content)
	out = b.stitchSpill(out, bl.ID)
	p.Output = &out
	addPart(m, parser.KindToolCall, p)
	for _, img := range images {
		b.addImage(m, img, r.raw)
	}
}

// childNativeID is the native id of the Child Session in subagents/agent-<agentID>
// (claude-code.md §3.5).
func childNativeID(session, agentID string) string { return session + "/agent-" + agentID }

func isToolResult(bl block) bool { return bl.Type == "tool_result" }

// agentID is the Child Session a tool result line reports running, from its
// toolUseResult.agentId, or "".
func agentID(l *line) string {
	var v struct {
		AgentID string `json:"agentId"`
	}
	if json.Unmarshal(l.ToolUseResult, &v) != nil {
		return ""
	}
	return v.AgentID
}

// resultContent is a tool_result's output text and its image blocks: a
// string as-is, or the text blocks joined by newlines with each
// tool_reference as a placeholder.
func resultContent(raw json.RawMessage) (string, []block) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var (
		texts  []string
		images []block
	)
	for _, bl := range contentBlocks(raw) {
		switch bl.Type {
		case "text":
			texts = append(texts, bl.Text)
		case "tool_reference":
			texts = append(texts, "[tool reference: "+bl.ToolName+"]")
		case "image":
			images = append(images, bl)
		}
	}
	return strings.Join(texts, "\n"), images
}

var spillRe = regexp.MustCompile(`Full output saved to: (\S+)`)

// stitchSpill replaces a <persisted-output> marker with the spilled file's
// content, when the Session's attachments hold it. Otherwise the marker, with
// its preview, stays.
func (b *builder) stitchSpill(out, callID string) string {
	if !strings.HasPrefix(strings.TrimSpace(out), "<persisted-output>") {
		return out
	}
	project, _, _ := strings.Cut(b.in.MainKey, "/")
	session, _, _ := strings.Cut(b.in.NativeID, "/")
	var keys []string
	if m := spillRe.FindStringSubmatch(out); m != nil {
		if i := strings.Index(m[1], "/tool-results/"); i >= 0 {
			// Keep the path from <session>/tool-results/ on.
			if j := strings.LastIndex(m[1][:i], "/"); j >= 0 {
				keys = append(keys, project+"/"+m[1][j+1:])
			}
		}
	}
	keys = append(keys, project+"/"+session+"/tool-results/"+callID+".txt")
	for _, k := range keys {
		if c, ok := b.in.Attachments[k]; ok {
			return strings.ToValidUTF8(string(c), "�")
		}
	}
	return out
}

// userKind is what a user line becomes (claude-code.md §3.3).
type userKind int

const (
	userMessage     userKind = iota // a user Message
	userToolResults                 // only tool results: no Message
	userSkip                        // injected context or command output: Raw only
	userCommand                     // a slash_command marker
	userShell                       // a shell_command marker
	userCompaction                  // a compaction marker with the summary
	userTask                        // a <task-notification> block: a task_notification marker when well formed
)

// userContent is a user line's message.content, or ok=false when the
// message doesn't decode.
func userContent(l *line) (content json.RawMessage, ok bool) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message, &m) != nil {
		return nil, false
	}
	return m.Content, true
}

// userBlocks returns a user line's content blocks, or nil for string content.
func userBlocks(l *line) []block {
	c, _ := userContent(l)
	return contentBlocks(c)
}

// userText is a user line's text: its string content, or its text blocks
// joined by newlines.
func userText(l *line) string {
	c, _ := userContent(l)
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var texts []string
	for _, b := range contentBlocks(c) {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

var (
	commandNameRe = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgsRe = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	bashInputRe   = regexp.MustCompile(`(?s)^<bash-input>(.*?)(?:</bash-input>|$)`)
	bashStdoutRe  = regexp.MustCompile(`(?s)<bash-stdout>(.*?)</bash-stdout>`)
	bashStderrRe  = regexp.MustCompile(`(?s)<bash-stderr>(.*?)</bash-stderr>`)
)

// isShellOutput reports whether s is a ! command's output line.
func isShellOutput(s string) bool {
	return strings.HasPrefix(s, "<bash-stdout>") || strings.HasPrefix(s, "<bash-stderr>")
}

// shellOutput is a ! command's output from its output line: stdout, then
// stderr, each only when non-empty, joined with a newline. Claude Code writes
// both bodies HTML-escaped, so they are unescaped. ok is false when l isn't
// an output line.
func shellOutput(l *line) (out string, ok bool) {
	if l.Type != "user" || l.IsMeta {
		return "", false
	}
	c, _ := userContent(l)
	var s string
	if json.Unmarshal(c, &s) != nil || !isShellOutput(s) {
		return "", false
	}
	var parts []string
	for _, re := range []*regexp.Regexp{bashStdoutRe, bashStderrRe} {
		if m := re.FindStringSubmatch(s); m != nil && m[1] != "" {
			parts = append(parts, html.UnescapeString(m[1]))
		}
	}
	return strings.Join(parts, "\n"), true
}

// commandText is a slash command's marker text: its name plus its
// arguments, e.g. "/review 42".
func commandText(s string) string {
	var name, args string
	if m := commandNameRe.FindStringSubmatch(s); m != nil {
		name = strings.TrimSpace(m[1])
	}
	if name != "" && !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if m := commandArgsRe.FindStringSubmatch(s); m != nil {
		args = strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(name + " " + args)
}

// classifyUser classifies a user line and returns its content blocks (string
// content comes back as one text block) or, for a marker, its text.
func classifyUser(l *line, warn *parser.Warnings) (kind userKind, blocks []block, text string) {
	if l.IsMeta {
		return userSkip, nil, ""
	}
	if l.IsCompactSummary {
		return userCompaction, nil, userText(l)
	}
	content, ok := userContent(l)
	if !ok {
		warn.Add(parser.WarnMissingField, "message", string(l.raw))
		return userSkip, nil, ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		// A skill invocation writes <command-message> before <command-name>;
		// both are the same slash-command line.
		if strings.HasPrefix(s, "<command-name>") || strings.HasPrefix(s, "<command-message>") {
			return userCommand, nil, commandText(s)
		}
		if m := bashInputRe.FindStringSubmatch(s); m != nil {
			return userShell, nil, "$ " + strings.TrimSpace(m[1])
		}
		// Output with its command before it is read with the command.
		if isShellOutput(s) {
			return userSkip, nil, ""
		}
		if l.Origin.Kind == taskMode || strings.HasPrefix(s, taskOpen) {
			return userTask, nil, s
		}
		for _, p := range []string{"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"} {
			if strings.HasPrefix(s, p) {
				return userSkip, nil, ""
			}
		}
		return userMessage, []block{{Type: "text", Text: s}}, ""
	}
	blocks = contentBlocks(content)
	if len(blocks) == 0 {
		warn.Add(parser.WarnMissingField, "message.content", string(l.raw))
		return userSkip, nil, ""
	}
	kind = userToolResults
	for _, b := range blocks {
		if b.Type != "tool_result" {
			kind = userMessage
		}
	}
	return kind, blocks, ""
}

// contentBlocks decodes a block list, keeping each block's raw JSON. It is
// nil for anything else.
func contentBlocks(raw json.RawMessage) []block {
	var rs []json.RawMessage
	if json.Unmarshal(raw, &rs) != nil {
		return nil
	}
	bs := make([]block, 0, len(rs))
	for _, r := range rs {
		var b block
		if json.Unmarshal(r, &b) != nil {
			b = block{Type: "?"}
		}
		b.raw = r
		bs = append(bs, b)
	}
	return bs
}
