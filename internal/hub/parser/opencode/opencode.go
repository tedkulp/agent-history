// Package opencode is the Hub parser for the opencode Source
// (docs/spec/adapters/opencode.md §2.5, §3). Both Layouts hold the same
// Session, Messages and parts: sqlite as a database export, legacy-json as
// one file each. Once loaded, both parse the same way.
package opencode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// version is the parser_version. Bump it whenever output changes for
// existing data (hub.md §4.5).
const version = 1

const (
	layoutSqlite = "sqlite"
	layoutLegacy = "legacy-json"
	dbPrefix     = "db:"
	jsonPrefix   = "json:"
)

// Parser is the opencode parser.
type Parser struct{}

// New returns the opencode parser.
func New() *Parser { return &Parser{} }

// Source implements parser.Parser.
func (*Parser) Source() string { return protocol.SourceOpencode }

// Version implements parser.Parser.
func (*Parser) Version() int { return version }

// MapKey implements parser.Parser (opencode.md §2.5).
func (*Parser) MapKey(key string) (parser.Mapping, bool) {
	if ses, ok := strings.CutPrefix(key, dbPrefix); ok {
		if !isID(ses, "ses_") {
			return parser.Mapping{}, false
		}
		return parser.Mapping{NativeID: ses, Role: parser.RoleMain, Layout: layoutSqlite, LayoutRank: 2}, true
	}
	rest, ok := strings.CutPrefix(key, jsonPrefix)
	if !ok {
		return parser.Mapping{}, false
	}
	segs := strings.Split(rest, "/")
	legacy := func(ses, role string) (parser.Mapping, bool) {
		return parser.Mapping{NativeID: ses, Role: role, Layout: layoutLegacy, LayoutRank: 1}, true
	}
	switch {
	case len(segs) == 3 && segs[0] == "session" && segs[1] != "" && isFile(segs[2], "ses_"):
		return legacy(strings.TrimSuffix(segs[2], ".json"), parser.RoleMain)
	case len(segs) == 3 && segs[0] == "message" && isID(segs[1], "ses_") && isFile(segs[2], "msg_"):
		return legacy(segs[1], parser.RoleAttachment)
	case len(segs) == 4 && segs[0] == "part" && isID(segs[1], "ses_") && isID(segs[2], "msg_") && isFile(segs[3], "prt_"):
		return legacy(segs[1], parser.RoleAttachment)
	}
	return parser.Mapping{}, false
}

// isID reports whether s is an opencode id with the given prefix.
func isID(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && len(s) > len(prefix) && !strings.ContainsAny(s, "/")
}

func isFile(s, prefix string) bool {
	id, ok := strings.CutSuffix(s, ".json")
	return ok && isID(id, prefix)
}

// sessionInfo is the Session's fields, from the session row or file.
type sessionInfo struct {
	title, version, dir, parent string
	created, updated            int64
	hasDir                      bool
	revert                      string // revert.messageID
	raw                         []byte
}

// message is one Message's data with its id.
type message struct {
	id    string
	data  msgData
	raw   []byte
	parts []*part
}

type msgData struct {
	Role string `json:"role"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Summary    json.RawMessage `json:"summary"`
	Tokens     *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// part is one part's data with its ids.
type part struct {
	id, msg string
	data    partData
	raw     []byte
}

type partData struct {
	Type      string   `json:"type"`
	Text      string   `json:"text"`
	Synthetic bool     `json:"synthetic"`
	Mime      string   `json:"mime"`
	URL       string   `json:"url"`
	Filename  string   `json:"filename"`
	Files     []string `json:"files"`
	Name      string   `json:"name"`
	Agent     string   `json:"agent"`
	Tool      string   `json:"tool"`
	CallID    string   `json:"callID"`
	State     *struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Output   *string         `json:"output"`
		Error    *string         `json:"error"`
		Metadata struct {
			SessionID string `json:"sessionId"`
		} `json:"metadata"`
	} `json:"state"`
}

// Parse implements parser.Parser.
func (*Parser) Parse(in parser.Input) (parser.Result, error) {
	var (
		res  parser.Result
		warn parser.Warnings
		s    sessionInfo
		msgs []*message
		prts []*part
	)
	if strings.HasPrefix(in.MainKey, dbPrefix) {
		s, msgs, prts = loadExport(in.Main, &warn)
	} else {
		s, msgs, prts = loadLegacy(in, &warn)
	}

	res.Session = parser.Session{
		Title:          s.title,
		StartedAt:      s.created,
		LastActivityAt: s.updated,
		Cwd:            s.dir,
		SourceVersion:  s.version,
		ParentNativeID: s.parent,
	}
	if !s.hasDir {
		warn.Add(parser.WarnMissingField, "directory", string(s.raw))
	}

	// Parts attach to their Message; both are in id order (opencode.md §3.2).
	slices.SortFunc(msgs, func(a, b *message) int { return strings.Compare(a.id, b.id) })
	byID := map[string]*message{}
	for _, m := range msgs {
		byID[m.id] = m
	}
	slices.SortFunc(prts, func(a, b *part) int { return strings.Compare(a.id, b.id) })
	for _, p := range prts {
		m, ok := byID[p.msg]
		if !ok {
			warn.Add(parser.WarnOrphan, "part", string(p.raw))
			continue
		}
		m.parts = append(m.parts, p)
	}

	for _, m := range msgs {
		if s.revert != "" && m.id >= s.revert {
			// The undone tail: Raw only until opencode deletes it.
			break
		}
		if pm, ok := buildMessage(m, &res, &warn); ok {
			res.Messages = append(res.Messages, pm)
		}
	}
	res.Warnings = warn.List()
	return res, nil
}

// loadExport reads a db: export, one table-tagged row per line (opencode.md §3.1).
func loadExport(b []byte, warn *parser.Warnings) (sessionInfo, []*message, []*part) {
	var s sessionInfo
	var msgs []*message
	var prts []*part
	for raw := range bytes.Lines(b) {
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
		case "session":
			var r struct {
				Title       string  `json:"title"`
				Version     string  `json:"version"`
				Directory   *string `json:"directory"`
				ParentID    *string `json:"parent_id"`
				Revert      *string `json:"revert"`
				TimeCreated int64   `json:"time_created"`
				TimeUpdated int64   `json:"time_updated"`
			}
			if err := json.Unmarshal(line.Row, &r); err != nil {
				warn.Add(parser.WarnBadLine, "session", string(raw))
				continue
			}
			s = sessionInfo{title: r.Title, version: r.Version, created: r.TimeCreated, updated: r.TimeUpdated, raw: raw}
			if r.Directory != nil && *r.Directory != "" {
				s.dir, s.hasDir = *r.Directory, true
			}
			if r.ParentID != nil {
				s.parent = *r.ParentID
			}
			if r.Revert != nil {
				s.revert = revertFrom([]byte(*r.Revert))
			}
		case "message", "part":
			var r struct {
				ID        string `json:"id"`
				MessageID string `json:"message_id"`
				Data      string `json:"data"`
			}
			if err := json.Unmarshal(line.Row, &r); err != nil || r.ID == "" {
				warn.Add(parser.WarnBadLine, line.Table, string(raw))
				continue
			}
			if line.Table == "message" {
				if m, ok := newMessage(r.ID, []byte(r.Data), warn); ok {
					msgs = append(msgs, m)
				}
			} else if p, ok := newPart(r.ID, r.MessageID, []byte(r.Data), warn); ok {
				prts = append(prts, p)
			}
		default:
			// session_message and any table opencode adds: Raw only.
		}
	}
	if s.raw == nil {
		s.hasDir = true
		warn.Add(parser.WarnMissingField, "session", "")
	}
	return s, msgs, prts
}

// loadLegacy reads a legacy-json Session: the session file as Main, its
// message and part files as attachments, told apart by key.
func loadLegacy(in parser.Input, warn *parser.Warnings) (sessionInfo, []*message, []*part) {
	var r struct {
		Title     string          `json:"title"`
		Version   string          `json:"version"`
		Directory string          `json:"directory"`
		ParentID  string          `json:"parentID"`
		Revert    json.RawMessage `json:"revert"`
		Time      struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	s := sessionInfo{raw: in.Main, hasDir: true}
	if err := json.Unmarshal(in.Main, &r); err != nil {
		warn.Add(parser.WarnBadLine, "session", string(in.Main))
	} else {
		s = sessionInfo{title: r.Title, version: r.Version, dir: r.Directory, parent: r.ParentID,
			created: r.Time.Created, updated: r.Time.Updated, hasDir: r.Directory != "", raw: in.Main}
		s.revert = revertFrom(r.Revert)
	}

	keys := make([]string, 0, len(in.Attachments))
	for k := range in.Attachments {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var msgs []*message
	var prts []*part
	for _, k := range keys {
		segs := strings.Split(strings.TrimPrefix(k, jsonPrefix), "/")
		id := strings.TrimSuffix(segs[len(segs)-1], ".json")
		b := in.Attachments[k]
		switch segs[0] {
		case "message":
			if m, ok := newMessage(id, b, warn); ok {
				msgs = append(msgs, m)
			}
		case "part":
			if p, ok := newPart(id, segs[2], b, warn); ok {
				prts = append(prts, p)
			}
		}
	}
	return s, msgs, prts
}

// revertFrom is revert.messageID from a Session's revert JSON, else empty.
func revertFrom(b []byte) string {
	var r struct {
		MessageID string `json:"messageID"`
	}
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.MessageID
}

func newMessage(id string, data []byte, warn *parser.Warnings) (*message, bool) {
	m := &message{id: id, raw: data}
	if err := json.Unmarshal(data, &m.data); err != nil {
		warn.Add(parser.WarnBadLine, "message", string(data))
		return nil, false
	}
	return m, true
}

func newPart(id, msg string, data []byte, warn *parser.Warnings) (*part, bool) {
	p := &part{id: id, msg: msg, raw: data}
	if err := json.Unmarshal(data, &p.data); err != nil {
		warn.Add(parser.WarnBadLine, "part", string(data))
		return nil, false
	}
	return p, true
}

// buildMessage maps a Message by role and its parts by type (opencode.md
// §3.3, §3.4). A Message left with no Parts is dropped.
func buildMessage(m *message, res *parser.Result, warn *parser.Warnings) (parser.Message, bool) {
	d := m.data
	pm := parser.Message{ID: parser.SafeID(m.id), Timestamp: d.Time.Created}
	switch d.Role {
	case "user":
		pm.Role = parser.MessageUser
	case "assistant":
		pm.Role = parser.MessageAssistant
		pm.Model, pm.Provider = d.ModelID, d.ProviderID
		if t := d.Tokens; t != nil {
			pm.Usage = &parser.Usage{Input: t.Input, Output: t.Output, Reasoning: t.Reasoning, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write}
		}
		if string(d.Summary) == "true" {
			// A compaction summary: its text becomes the marker.
			var texts []string
			for _, p := range m.parts {
				if p.data.Type == "text" && !p.data.Synthetic && strings.TrimSpace(p.data.Text) != "" {
					texts = append(texts, p.data.Text)
				}
			}
			text := strings.Join(texts, "\n\n")
			if text == "" {
				text = parser.CompactionText
			}
			addMarker(&pm, parser.MarkerCompaction, text)
			return pm, true
		}
	default:
		pm.Role = parser.MessageAssistant
		warn.Add(parser.WarnUnknownType, "message:"+d.Role, string(m.raw))
		addPart(&pm, "", parser.KindUnknown, parser.NewUnknown("message:"+d.Role, m.raw))
		return pm, true
	}
	for _, p := range m.parts {
		buildPart(&pm, p, res, warn)
	}
	if e := d.Error; e != nil && pm.Role == parser.MessageAssistant {
		text := "Error: " + e.Name
		if e.Data.Message != "" {
			text += ": " + e.Data.Message
		}
		addPart(&pm, "", parser.KindText, parser.TextPayload{Text: text})
	}
	return pm, len(pm.Parts) > 0
}

// addPart adds a Part with its Source id, or <message id>.<index> without one.
func addPart(m *parser.Message, id, kind string, payload any) {
	if id == "" {
		id = m.ID + "." + strconv.Itoa(len(m.Parts))
	} else {
		id = parser.SafeID(id)
	}
	m.Parts = append(m.Parts, parser.Part{ID: id, Kind: kind, Payload: payload})
}

func addMarker(m *parser.Message, marker, text string) {
	addPart(m, "", parser.KindMarker, parser.MarkerPayload{Marker: marker, Text: text})
}

func buildPart(m *parser.Message, p *part, res *parser.Result, warn *parser.Warnings) {
	d := p.data
	switch d.Type {
	case "text":
		if !d.Synthetic {
			addPart(m, p.id, parser.KindText, parser.TextPayload{Text: d.Text})
		}
	case "reasoning":
		if strings.TrimSpace(d.Text) != "" {
			addPart(m, p.id, parser.KindThinking, parser.ThinkingPayload{Text: d.Text})
		}
	case "tool":
		addPart(m, p.id, parser.KindToolCall, toolCall(d))
	case "file":
		if data, ok := strings.CutPrefix(d.URL, "data:"); ok && strings.HasPrefix(d.Mime, "image/") {
			if _, b64, ok := strings.Cut(data, ","); ok {
				if b, err := base64.StdEncoding.DecodeString(b64); err == nil && len(b) > 0 {
					payload, img := parser.NewImage(d.Mime, b)
					payload.Alt = d.Filename
					res.Images = append(res.Images, img)
					addPart(m, p.id, parser.KindImage, payload)
					return
				}
			}
			warn.Add(parser.WarnMissingField, "file.url", string(p.raw))
		}
		label := d.Filename
		if label == "" {
			label = d.URL
		}
		addPart(m, p.id, parser.KindAttachment, parser.AttachmentPayload{Label: label})
	case "patch":
		addPart(m, p.id, parser.KindAttachment, parser.AttachmentPayload{Label: "patch: " + plural(len(d.Files), "file")})
	case "snapshot":
		addPart(m, p.id, parser.KindAttachment, parser.AttachmentPayload{Label: "snapshot"})
	case "agent", "subtask":
		name := d.Name
		if name == "" {
			name = d.Agent
		}
		addPart(m, p.id, parser.KindAttachment, parser.AttachmentPayload{Label: "@" + name})
	case "compaction":
		addPart(m, p.id, parser.KindMarker, parser.MarkerPayload{Marker: parser.MarkerCompaction, Text: parser.CompactionText})
	case "step-start", "step-finish", "retry":
		// Step bookkeeping: Raw only.
	default:
		warn.Add(parser.WarnUnknownType, d.Type, string(p.raw))
		addPart(m, p.id, parser.KindUnknown, parser.NewUnknown(d.Type, p.raw))
	}
}

func plural(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n != 1 {
		s += "s"
	}
	return s
}

// toolCall maps a tool part, a call and its result in one (opencode.md §3.5).
func toolCall(d partData) parser.ToolCallPayload {
	tc := parser.ToolCallPayload{CallID: d.CallID, Name: d.Tool, Input: json.RawMessage("null"), Status: parser.StatusPending, ChildSessions: []string{}}
	st := d.State
	if st == nil {
		return tc
	}
	if len(st.Input) > 0 {
		tc.Input = st.Input
	}
	switch st.Status {
	case "completed":
		tc.Status = parser.StatusOK
		out := ""
		if st.Output != nil {
			out = *st.Output
		}
		tc.Output = &out
	case "error":
		tc.Status = parser.StatusError
		out := ""
		if st.Error != nil {
			out = *st.Error
		}
		tc.Output = &out
	}
	switch d.Tool {
	case "edit":
		var in struct {
			FilePath  string `json:"filePath"`
			OldString string `json:"oldString"`
			NewString string `json:"newString"`
		}
		if json.Unmarshal(tc.Input, &in) == nil && in.FilePath != "" {
			tc.Diff = &parser.Diff{Path: in.FilePath, Old: in.OldString, New: in.NewString}
		}
	case "task":
		if id := st.Metadata.SessionID; id != "" {
			tc.ChildSessions = []string{id}
		}
	}
	return tc
}
