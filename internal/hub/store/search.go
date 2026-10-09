package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// Search row kinds (hub.md §3.7).
const (
	searchKindMessage = "message"
	searchKindTitle   = "title"
)

// A Session's search rows get rowids from sessionID<<searchRowBits: the
// title row first, then one per Message by ordinal. session_id is an
// UNINDEXED column, so deleting by it would scan the whole table; a rowid
// range is a direct lookup.
const (
	searchRowBits = 24
	maxSearchRows = 1<<searchRowBits - 1 // Messages per Session that get indexed
)

func searchRowID(sessionID int64, n int) int64 { return sessionID<<searchRowBits | int64(n) }

// deleteSearchRows removes a Session's search rows.
func deleteSearchRows(ctx context.Context, tx execer, sessionID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM search WHERE rowid BETWEEN ? AND ?`,
		searchRowID(sessionID, 0), searchRowID(sessionID, maxSearchRows))
	return err
}

// insertSearchTitle writes a Session's title row.
func insertSearchTitle(ctx context.Context, tx execer, sessionID int64, title string) error {
	if title == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO search (rowid, body, kind, session_id, message_id) VALUES (?, ?, ?, ?, NULL)`,
		searchRowID(sessionID, 0), title, searchKindTitle, sessionID)
	return err
}

// insertSearchMessage writes one Message's search row: its text Parts joined
// by newlines, plus each Tool call's name and compact JSON input. Tool
// output, thinking, markers, unknown Parts and attachments aren't indexed
// (hub.md §3.7).
func insertSearchMessage(ctx context.Context, tx execer, sessionID int64, ordinal int, m parser.Message) error {
	if ordinal+1 > maxSearchRows {
		return nil
	}
	var lines []string
	for _, p := range m.Parts {
		switch pl := p.Payload.(type) {
		case parser.TextPayload:
			lines = append(lines, pl.Text)
		case parser.ToolCallPayload:
			line := pl.Name
			var in bytes.Buffer
			if len(pl.Input) > 0 && json.Compact(&in, pl.Input) == nil && in.String() != "null" {
				line += " " + in.String()
			}
			lines = append(lines, line)
		}
	}
	body := strings.TrimSpace(strings.Join(lines, "\n"))
	if body == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO search (rowid, body, kind, session_id, message_id) VALUES (?, ?, ?, ?, ?)`,
		searchRowID(sessionID, ordinal+1), body, searchKindMessage, sessionID, m.ID)
	return err
}

// ParseQuery splits search box input into terms: whitespace-separated words,
// with "quoted phrases" kept together. FTS5 operator characters become
// spaces, and a term left with no letter or digit is dropped, since it
// could match nothing. No terms means the input isn't a search (hub.md §3.7).
func ParseQuery(q string) []string {
	var (
		terms  []string
		cur    strings.Builder
		quoted bool
	)
	flush := func() {
		t := strings.Join(strings.FieldsFunc(cur.String(), func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(ftsOperators, r)
		}), " ")
		cur.Reset()
		if strings.IndexFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) >= 0 {
			terms = append(terms, t)
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			flush()
			quoted = !quoted
		case unicode.IsSpace(r) && !quoted:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

// ftsOperators are the characters FTS5 query syntax gives a meaning to.
const ftsOperators = `"*():^{}+-`

// matchExpr is the FTS5 MATCH expression for terms: each one quoted, ANDed,
// the last one a prefix match.
func matchExpr(terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	return strings.Join(quoted, " AND ") + "*"
}

// Hit is one search result: a Message, or a Session's title.
type Hit struct {
	SessionID   int64
	Source      string
	NativeID    string
	Title       string
	MachineID   string
	Machine     string
	ProjectCwd  string
	MessageID   string // "" for a title hit
	Role        string // the Message's role; "" for a title hit
	Snippet     string // raw text, each match between SnippetOpen and SnippetClose
	Timestamp   int64  // the Message's time, or the Session's last activity for a title hit; Unix ms, 0 when unknown
	ParentID    int64  // set for a hit in a Child Session
	ParentTitle string
}

// Snippet sentinels around each match: private-use runes that Transcript
// text is vanishingly unlikely to hold. The web layer escapes the snippet,
// then turns them into <mark> tags (hub.md §3.7).
const (
	SnippetOpen  = "\uE000"
	SnippetClose = "\uE001"
)

// snippetTokens is how many tokens a snippet spans (hub.md §3.7).
const snippetTokens = 24

// searchFrom joins each search row to its Session (s) and Machine (m).
const searchFrom = `search
	JOIN sessions s ON s.id = search.session_id
	JOIN machines m ON m.id = s.machine_id`

// searchWhere is the WHERE clause and args for a search over terms with f's
// chips. The visible Sessions are the parsed ones, Child Sessions included.
func searchWhere(terms []string, f FeedFilter) (string, []any) {
	where := []string{`search MATCH ?`, `s.parsed_at IS NOT NULL`}
	args := []any{matchExpr(terms)}
	w, a := chipWhere(f)
	return strings.Join(append(where, w...), " AND "), append(args, a...)
}

// Search returns the hits for terms with f's chips, best first: at most
// limit of them after skipping offset (hub.md §4.7). f.Before is ignored,
// since hits are ranked, not dated.
func (s *Store) Search(ctx context.Context, terms []string, f FeedFilter, offset, limit int) ([]Hit, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	where, args := searchWhere(terms, f)
	rows, err := s.read.QueryContext(ctx, `
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), m.id, `+machineLabel+`, coalesce(s.project_cwd, ''),
			coalesce(search.message_id, ''), coalesce(msg.role, ''),
			snippet(search, 0, ?, ?, '…', ?), coalesce(msg.timestamp, s.last_activity_at, 0),
			coalesce(p.id, 0), coalesce(p.title, p.native_id, '')
		FROM `+searchFrom+`
		LEFT JOIN messages msg ON msg.session_id = s.id AND msg.id = search.message_id
		LEFT JOIN sessions p ON p.id = s.parent_session_id
		WHERE `+where+`
		ORDER BY bm25(search) * CASE search.kind WHEN ? THEN 2.0 ELSE 1.0 END, s.id, search.rowid
		LIMIT ? OFFSET ?`,
		append(append([]any{SnippetOpen, SnippetClose, snippetTokens}, args...), searchKindTitle, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.SessionID, &h.Source, &h.NativeID, &h.Title, &h.MachineID, &h.Machine, &h.ProjectCwd,
			&h.MessageID, &h.Role, &h.Snippet, &h.Timestamp, &h.ParentID, &h.ParentTitle); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Facet is one facet value with its hit count.
type Facet struct {
	MachineID string // for Project facets, the Project's Machine
	Machine   string // the Machine's label
	Value     string // Machine id, Source, or project_cwd ("" = No project)
	Hits      int
}

// Facets are the hit counts of a search per Machine, Source and Project
// (hub.md §4.7).
type Facets struct {
	Machines []Facet
	Sources  []Facet
	Projects []Facet
}

// SearchFacets counts the hits for terms per Machine, Source and Project,
// most hits first.
func (s *Store) SearchFacets(ctx context.Context, terms []string, f FeedFilter) (Facets, error) {
	var out Facets
	if len(terms) == 0 {
		return out, nil
	}
	where, args := searchWhere(terms, f)
	for _, q := range []struct {
		dst  *[]Facet
		cols string
	}{
		{&out.Machines, `m.id, ` + machineLabel + `, m.id`},
		{&out.Sources, `'', '', s.source`},
		{&out.Projects, `m.id, ` + machineLabel + `, coalesce(s.project_cwd, '')`},
	} {
		rows, err := s.read.QueryContext(ctx, fmt.Sprintf(`
			SELECT %s, count(*) AS n
			FROM `+searchFrom+`
			WHERE %s
			GROUP BY 1, 3
			ORDER BY n DESC, 2, 3`, q.cols, where), args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var fc Facet
			if err := rows.Scan(&fc.MachineID, &fc.Machine, &fc.Value, &fc.Hits); err != nil {
				rows.Close()
				return out, err
			}
			*q.dst = append(*q.dst, fc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}
