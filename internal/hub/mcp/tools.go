package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/web"
)

// Session is a past Session as results quote it.
type Session struct {
	SessionID int64  `json:"session_id" jsonschema:"the Hub's id for the Session, which get_transcript takes"`
	NativeID  string `json:"native_id" jsonschema:"the Source's own id for the Session, to resume it there"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	Machine   string `json:"machine"`
	Project   string `json:"project,omitempty" jsonschema:"the working directory it started in; empty for no Project"`
	Date      string `json:"date,omitempty" jsonschema:"its last activity; for a search hit, when the Message was written"`
	URL       string `json:"url" jsonschema:"its Transcript in the Hub's web UI"`
}

// session quotes a Session; an untitled one goes by its native id.
func session(base string, id int64, native, title, source, machine, project string, at int64) Session {
	return Session{
		SessionID: id, NativeID: native, Title: cmp.Or(title, native), Source: source,
		Machine: machine, Project: project, Date: date(at), URL: sessionURL(base, id),
	}
}

// SessionLink names another Session.
type SessionLink struct {
	SessionID int64  `json:"session_id"`
	Title     string `json:"title"`
}

// SearchInput is search's arguments.
type SearchInput struct {
	Query string `json:"query" jsonschema:"words to find; a \"quoted phrase\" stays together and the last word matches as a prefix"`
	Filters
}

// Hit is one search hit: a Message, or a Session's title.
type Hit struct {
	Session
	MessageID string       `json:"message_id,omitempty" jsonschema:"the Message hit; empty when the title matched"`
	Role      string       `json:"role,omitempty"`
	Snippet   string       `json:"snippet" jsonschema:"the matching text, each match between ** marks"`
	Parent    *SessionLink `json:"parent,omitempty" jsonschema:"the Session whose Tool call spawned this Child Session"`
}

// Facet is one filter value with its hit count.
type Facet struct {
	Value   string `json:"value"`
	Machine string `json:"machine,omitempty" jsonschema:"for a project, its Machine"`
	Hits    int    `json:"hits"`
}

// Facets are the hit counts per filter value.
type Facets struct {
	Machines []Facet `json:"machines"`
	Sources  []Facet `json:"sources"`
	Projects []Facet `json:"projects" jsonschema:"working directories; pass one as cwd"`
}

// SearchOutput is search's result.
type SearchOutput struct {
	Note   string `json:"note"`
	Hits   []Hit  `json:"hits"`
	Facets Facets `json:"facets"`
}

func (t *tools) search(ctx context.Context, req *gomcp.CallToolRequest, in SearchInput) (*gomcp.CallToolResult, SearchOutput, error) {
	out := SearchOutput{Note: quoted, Hits: []Hit{}, Facets: Facets{Machines: []Facet{}, Sources: []Facet{}, Projects: []Facet{}}}
	terms := store.ParseQuery(in.Query)
	if len(terms) == 0 {
		return nil, out, errors.New("query has no words to search for")
	}
	f, err := t.feedFilter(ctx, in.Filters)
	if err != nil {
		return nil, out, err
	}
	hits, err := t.store.Search(ctx, terms, f, 0, limit(in.Limit, defaultLimit, maxLimit))
	if err != nil {
		return nil, out, t.fail("search", err)
	}
	facets, err := t.store.SearchFacets(ctx, terms, f)
	if err != nil {
		return nil, out, t.fail("search", err)
	}
	base := baseURL(req)
	snippet := strings.NewReplacer(store.SnippetOpen, "**", store.SnippetClose, "**")
	for _, h := range hits {
		hit := Hit{
			Session:   session(base, h.SessionID, h.NativeID, h.Title, h.Source, h.Machine, h.ProjectCwd, h.Timestamp),
			MessageID: h.MessageID, Role: h.Role, Snippet: snippet.Replace(h.Snippet),
		}
		if h.MessageID != "" {
			hit.URL += "#m-" + url.PathEscape(h.MessageID)
		}
		if h.ParentID != 0 {
			hit.Parent = &SessionLink{SessionID: h.ParentID, Title: h.ParentTitle}
		}
		out.Hits = append(out.Hits, hit)
	}
	for _, fc := range facets.Machines {
		out.Facets.Machines = append(out.Facets.Machines, Facet{Value: fc.Machine, Hits: fc.Hits})
	}
	for _, fc := range facets.Sources {
		out.Facets.Sources = append(out.Facets.Sources, Facet{Value: fc.Value, Hits: fc.Hits})
	}
	for _, fc := range facets.Projects {
		if fc.Value != "" {
			out.Facets.Projects = append(out.Facets.Projects, Facet{Value: fc.Value, Machine: fc.Machine, Hits: fc.Hits})
		}
	}
	return nil, out, nil
}

// ListInput is list_sessions' arguments.
type ListInput struct {
	Filters
	Before string `json:"before,omitempty" jsonschema:"the next of the previous page, for the page after it"`
}

// ListOutput is list_sessions' result.
type ListOutput struct {
	Note     string    `json:"note"`
	Sessions []Session `json:"sessions"`
	Next     string    `json:"next,omitempty" jsonschema:"pass as before for the next page; absent on the last page"`
}

func (t *tools) listSessions(ctx context.Context, req *gomcp.CallToolRequest, in ListInput) (*gomcp.CallToolResult, ListOutput, error) {
	out := ListOutput{Note: quoted, Sessions: []Session{}}
	f, err := t.feedFilter(ctx, in.Filters)
	if err != nil {
		return nil, out, err
	}
	if in.Before != "" {
		at, id, ok := strings.Cut(in.Before, ":")
		c := store.Cursor{}
		var errAt, errID error
		c.At, errAt = strconv.ParseInt(at, 10, 64)
		c.ID, errID = strconv.ParseInt(id, 10, 64)
		if !ok || errAt != nil || errID != nil {
			return nil, out, fmt.Errorf("before %q is not a next from list_sessions", in.Before)
		}
		f.Before = &c
	}
	n := limit(in.Limit, defaultLimit, maxLimit)
	rows, err := t.store.Feed(ctx, f, n)
	if err != nil {
		return nil, out, t.fail("list_sessions", err)
	}
	base := baseURL(req)
	for _, r := range rows {
		out.Sessions = append(out.Sessions, session(base, r.ID, r.NativeID, r.Title, r.Source, r.Machine, r.ProjectCwd, r.LastActivityAt))
	}
	if len(rows) == n {
		last := rows[len(rows)-1]
		out.Next = fmt.Sprintf("%d:%d", last.LastActivityAt, last.ID)
	}
	return nil, out, nil
}

// TranscriptInput is get_transcript's arguments.
type TranscriptInput struct {
	SessionID         int64  `json:"session_id" jsonschema:"the Session, by the session_id from search or list_sessions"`
	Around            string `json:"around,omitempty" jsonschema:"a message_id: return the Messages around it; not with from"`
	Context           int    `json:"context,omitempty" jsonschema:"with around, how many Messages before and after it: default 10, at most 50"`
	From              *int   `json:"from,omitempty" jsonschema:"the position of the first Message to return, 0 for the start, or a previous next"`
	Limit             int    `json:"limit,omitempty" jsonschema:"without around, most Messages to return: default 50, at most 100"`
	IncludeToolOutput bool   `json:"include_tool_output,omitempty" jsonschema:"include each Tool call's output, cut to about 2 KB"`
	IncludeThinking   bool   `json:"include_thinking,omitempty" jsonschema:"include the assistant's thinking"`
}

// TranscriptOutput is get_transcript's result.
type TranscriptOutput struct {
	Note       string       `json:"note"`
	Session    Session      `json:"session"`
	Parent     *SessionLink `json:"parent,omitempty" jsonschema:"the Session whose Tool call spawned this Child Session"`
	ParseError string       `json:"parse_error,omitempty" jsonschema:"set when the Hub couldn't parse the Session's latest data"`
	Total      int          `json:"total_messages"`
	Messages   []Message    `json:"messages"`
	Next       *int         `json:"next,omitempty" jsonschema:"pass as from for the Messages after these; absent at the end"`
}

// Message is one Message of a Transcript.
type Message struct {
	MessageID string `json:"message_id"`
	Position  int    `json:"position"`
	Role      string `json:"role"`
	Date      string `json:"date,omitempty"`
	Content   string `json:"content"`
}

// toolOutputMax is the most bytes of a tool call's output a Transcript
// includes.
const toolOutputMax = 2 << 10

func (t *tools) getTranscript(ctx context.Context, req *gomcp.CallToolRequest, in TranscriptInput) (*gomcp.CallToolResult, TranscriptOutput, error) {
	out := TranscriptOutput{Note: quoted, Messages: []Message{}}
	if in.Around != "" && in.From != nil {
		return nil, out, errors.New("pass around or from, not both")
	}
	from, n := 0, limit(in.Limit, defaultPage, maxLimit)
	if in.From != nil {
		from = max(*in.From, 0)
	}
	if in.Around != "" {
		at, ok, err := t.store.MessageOrdinal(ctx, in.SessionID, in.Around)
		if err != nil {
			return nil, out, t.fail("get_transcript", err)
		}
		if !ok {
			return nil, out, fmt.Errorf("Session %d has no Message %q", in.SessionID, in.Around)
		}
		c := limit(in.Context, defaultAround, maxAround)
		from, n = max(at-c, 0), at+c+1-max(at-c, 0)
	}
	h, msgs, total, ok, err := t.store.TranscriptRange(ctx, in.SessionID, from, n)
	if err != nil {
		return nil, out, t.fail("get_transcript", err)
	}
	if !ok {
		return nil, out, fmt.Errorf("no Session %d", in.SessionID)
	}
	base := baseURL(req)
	out.Session = session(base, h.ID, h.NativeID, h.Title, h.Source, h.Machine, h.ProjectCwd, h.StartedAt)
	if h.Parent != nil {
		out.Parent = &SessionLink{SessionID: h.Parent.ID, Title: cmp.Or(h.Parent.Title, h.Parent.NativeID)}
	}
	out.ParseError, out.Total = h.ParseError, total
	for i, m := range msgs {
		content, err := t.render(ctx, h, m.Parts, in)
		if err != nil {
			return nil, out, t.fail("get_transcript", err)
		}
		out.Messages = append(out.Messages, Message{MessageID: m.ID, Position: from + i, Role: m.Role, Date: date(m.Timestamp), Content: content})
	}
	if next := from + len(msgs); len(msgs) > 0 && next < total {
		out.Next = &next
	}
	return nil, out, nil
}

// render writes a Message's Parts as text: text in full; a tool call as its
// name and a one-line input summary, its output only when asked for; thinking
// only when asked for; images and attachments as a placeholder line.
func (t *tools) render(ctx context.Context, h store.SessionHeader, parts []store.TranscriptPart, in TranscriptInput) (string, error) {
	var b strings.Builder
	line := func(format string, a ...any) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, format, a...)
	}
	for _, p := range parts {
		switch p.Kind {
		case parser.KindText:
			var tp parser.TextPayload
			if t.decode(h.ID, p, &tp) {
				line("%s", tp.Text)
			}
		case parser.KindThinking:
			var tp parser.ThinkingPayload
			if in.IncludeThinking && t.decode(h.ID, p, &tp) {
				line("[thinking]\n%s\n[/thinking]", tp.Text)
			}
		case parser.KindToolCall:
			var tc parser.ToolCallPayload
			if !t.decode(h.ID, p, &tc) {
				continue
			}
			summary, target := web.CallSummary(tc.Description, tc.Input)
			call := "[tool call] " + tc.Name
			if summary != "" {
				call += ": " + summary
			}
			if target != "" {
				call += " (" + target + ")"
			}
			if tc.Status == parser.StatusError {
				call += " [failed]"
			}
			for _, c := range tc.ChildSessions {
				if id, ok := h.CallChildren[c]; ok {
					call += fmt.Sprintf(" [spawned Child Session %d]", id)
				}
			}
			line("%s", call)
			if in.IncludeToolOutput {
				o, err := t.toolOutput(ctx, h.ID, p.ID, tc)
				if err != nil {
					return "", err
				}
				if o != "" {
					line("[output]\n%s\n[/output]", o)
				}
			}
		case parser.KindMarker:
			var mp parser.MarkerPayload
			if t.decode(h.ID, p, &mp) {
				line("[%s] %s", mp.Marker, mp.Text)
			}
		case parser.KindImage:
			line("[image]")
		case parser.KindAttachment:
			var ap parser.AttachmentPayload
			t.decode(h.ID, p, &ap)
			line("[attachment: %s]", ap.Label)
		case parser.KindUnknown:
			var up parser.UnknownPayload
			t.decode(h.ID, p, &up)
			line("[unrecognised %s]", up.SourceType)
		}
	}
	return b.String(), nil
}

// decode decodes a Part's payload into v, logging a bad one.
func (t *tools) decode(sessionID int64, p store.TranscriptPart, v any) bool {
	if err := json.Unmarshal([]byte(p.Payload), v); err != nil {
		t.log.Warn("bad "+p.Kind+" payload", "session", sessionID, "part", p.ID, "err", err)
		return false
	}
	return true
}

// toolOutput is a Tool call's output cut to toolOutputMax, saying how much
// was left out.
func (t *tools) toolOutput(ctx context.Context, sessionID int64, partID string, tc parser.ToolCallPayload) (string, error) {
	var o string
	switch {
	case tc.Output != nil:
		o = *tc.Output
	case tc.OutputSize > 0:
		// Large output lives apart from the payload (hub.md §3.5).
		full, _, err := t.store.ToolOutput(ctx, sessionID, partID)
		if err != nil {
			return "", err
		}
		o = full
	}
	cut := parser.CutBytes(o, toolOutputMax)
	if n := len(o) - len(cut); n > 0 {
		cut += fmt.Sprintf("\n… %d more bytes", n)
	}
	return cut, nil
}
