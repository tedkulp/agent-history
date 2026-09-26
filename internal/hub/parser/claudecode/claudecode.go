// Package claudecode is the Hub parser for the claude-code Source
// (docs/spec/adapters/claude-code.md §2.3, §3).
package claudecode

import (
	"bytes"
	"encoding/json"
	"regexp"
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

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	agentRe = regexp.MustCompile(`^agent-([^/.]+)\.(jsonl|meta\.json)$`)
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
		if a := agentRe.FindStringSubmatch(segs[3]); a != nil {
			role := parser.RoleMain
			if a[2] == "meta.json" {
				role = parser.RoleAttachment
			}
			return m(name+"/agent-"+a[1], role)
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
	Timestamp         string          `json:"timestamp"`
	Cwd               string          `json:"cwd"`
	GitBranch         string          `json:"gitBranch"`
	Version           string          `json:"version"`
	Message           json.RawMessage `json:"message"`
	CustomTitle       string          `json:"customTitle"`
	AITitle           string          `json:"aiTitle"`
	Summary           string          `json:"summary"`

	raw []byte
	ts  int64
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

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
	res.Session.Title = firstNonEmpty(customTitle, titleFromFile(in), aiTitle, summary)

	res.Messages = buildMessages(transcriptPath(lines, isChild, &warn))
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
	return path
}

// buildMessages turns path lines into Messages (claude-code.md §3.3).
// Assistant lines sharing a message.id form one Message; lines that produce
// no Message (tool results, injected context) don't break the group.
func buildMessages(path []*line) []parser.Message {
	var (
		msgs      []parser.Message
		cur       *parser.Message
		curAPIID  string
		lastUsage *parser.Usage
	)
	flush := func() {
		if cur != nil {
			cur.Usage = lastUsage
			msgs = append(msgs, *cur)
		}
		cur, curAPIID, lastUsage = nil, "", nil
	}
	for _, l := range path {
		switch l.Type {
		case "user":
			texts, ok := userTexts(l)
			if !ok {
				continue
			}
			flush()
			m := parser.Message{ID: parser.SafeID(l.UUID), Role: parser.MessageUser, Timestamp: l.ts}
			for _, t := range texts {
				addText(&m, t)
			}
			msgs = append(msgs, m)
		case "assistant":
			var am apiMessage
			if json.Unmarshal(l.Message, &am) != nil {
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
			if u := am.Usage; u != nil {
				lastUsage = &parser.Usage{
					Input:      u.InputTokens,
					Output:     u.OutputTokens,
					CacheRead:  u.CacheReadInputTokens,
					CacheWrite: u.CacheCreationInputTokens,
					Reasoning:  u.OutputTokensDetails.ThinkingTokens,
				}
			}
			for _, b := range contentBlocks(am.Content) {
				if b.Type == "text" {
					addText(cur, b.Text)
				}
			}
		}
	}
	flush()
	return msgs
}

func addText(m *parser.Message, text string) {
	m.Parts = append(m.Parts, parser.Part{
		ID:      m.ID + "." + strconv.Itoa(len(m.Parts)),
		Kind:    parser.KindText,
		Payload: parser.TextPayload{Text: text},
	})
}

// userTexts returns the text of a user line that becomes a user Message, or
// ok=false for one that doesn't (claude-code.md §3.3).
func userTexts(l *line) (texts []string, ok bool) {
	if l.IsMeta || l.IsCompactSummary {
		return nil, false
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message, &m) != nil {
		return nil, false
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		// A skill invocation writes <command-message> before <command-name>;
		// both are the same slash-command line.
		for _, p := range []string{"<command-name>", "<command-message>", "<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"} {
			if strings.HasPrefix(s, p) {
				return nil, false
			}
		}
		return []string{s}, true
	}
	blocks := contentBlocks(m.Content)
	if len(blocks) == 0 {
		return nil, false
	}
	onlyResults := true
	for _, b := range blocks {
		if b.Type != "tool_result" {
			onlyResults = false
		}
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return texts, !onlyResults
}

func contentBlocks(raw json.RawMessage) []block {
	var bs []block
	json.Unmarshal(raw, &bs)
	return bs
}
