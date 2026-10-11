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
	return s.searchFacets(ctx, terms, f, `count(*)`)
}

// SearchSessionFacets counts the Sessions with hits for terms per Machine,
// Source and Project, most Sessions first (hub.md §4.7).
func (s *Store) SearchSessionFacets(ctx context.Context, terms []string, f FeedFilter) (Facets, error) {
	return s.searchFacets(ctx, terms, f, `count(DISTINCT s.id)`)
}

// searchFacets groups the hits for terms per Machine, Source and Project,
// counting each group with count.
func (s *Store) searchFacets(ctx context.Context, terms []string, f FeedFilter, count string) (Facets, error) {
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
			SELECT %s, %s AS n
			FROM `+searchFrom+`
			WHERE %s
			GROUP BY 1, 3
			ORDER BY n DESC, 2, 3`, q.cols, count, where), args...)
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

// SearchTopHits is how many of its best Message hits a Session of a grouped
// search shows (hub.md §4.7).
const SearchTopHits = 3

// SessionHits is one Session of a grouped search, with its best hits.
type SessionHits struct {
	SessionID      int64
	Source         string
	NativeID       string
	Title          string
	MachineID      string
	Machine        string
	ProjectCwd     string
	LastActivityAt int64 // Unix ms, 0 when unknown
	ParentID       int64 // set for a Child Session
	ParentTitle    string
	Hits           int   // all its hits, the title hit included
	TitleHit       bool  // whether its title is a hit
	Top            []Hit // its best Message hits, at most SearchTopHits, best first
}

// SearchSessions returns the Sessions with hits for terms and f's chips,
// each ranked by its best hit as Search ranks hits: at most limit of them
// after skipping offset (hub.md §4.7). f.Before is ignored.
func (s *Store) SearchSessions(ctx context.Context, terms []string, f FeedFilter, offset, limit int) ([]SessionHits, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	where, args := searchWhere(terms, f)
	rows, err := s.read.QueryContext(ctx, `
		WITH h AS MATERIALIZED (
			SELECT search.session_id AS sid, search.kind AS kind,
				bm25(search) * CASE search.kind WHEN ? THEN 2.0 ELSE 1.0 END AS score
			FROM `+searchFrom+`
			WHERE `+where+`
		), g AS (
			SELECT sid, min(score) AS best, count(*) AS n, max(kind = ?) AS title_hit FROM h GROUP BY sid
		)
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), m.id, `+machineLabel+`, coalesce(s.project_cwd, ''),
			coalesce(s.last_activity_at, 0), coalesce(p.id, 0), coalesce(p.title, p.native_id, ''), g.n, g.title_hit
		FROM g
		JOIN sessions s ON s.id = g.sid
		JOIN machines m ON m.id = s.machine_id
		LEFT JOIN sessions p ON p.id = s.parent_session_id
		ORDER BY g.best, s.id
		LIMIT ? OFFSET ?`,
		append(append([]any{searchKindTitle}, args...), searchKindTitle, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionHits
	for rows.Next() {
		var g SessionHits
		if err := rows.Scan(&g.SessionID, &g.Source, &g.NativeID, &g.Title, &g.MachineID, &g.Machine, &g.ProjectCwd,
			&g.LastActivityAt, &g.ParentID, &g.ParentTitle, &g.Hits, &g.TitleHit); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	rows.Close()

	// Each Session's best Message hits, ranked as Search ranks them.
	var ids []int64
	at := map[int64]*SessionHits{}
	for i := range out {
		ids = append(ids, out[i].SessionID)
		at[out[i].SessionID] = &out[i]
	}
	idList, _ := json.Marshal(ids)
	rows, err = s.read.QueryContext(ctx, `
		WITH h AS MATERIALIZED (
			SELECT search.rowid AS rid, search.session_id AS sid, bm25(search) AS score
			FROM search
			WHERE search MATCH ? AND search.kind = ? AND search.session_id IN (SELECT value FROM json_each(?))
		)
		SELECT rid, sid FROM (
			SELECT rid, sid, row_number() OVER (PARTITION BY sid ORDER BY score, rid) AS rn FROM h
		)
		WHERE rn <= ?
		ORDER BY sid, rn`,
		matchExpr(terms), searchKindMessage, string(idList), SearchTopHits)
	if err != nil {
		return nil, err
	}
	var rids []int64
	owner := map[int64]int64{}
	for rows.Next() {
		var rid, sid int64
		if err := rows.Scan(&rid, &sid); err != nil {
			rows.Close()
			return nil, err
		}
		rids = append(rids, rid)
		owner[rid] = sid
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits, err := s.hitsAt(ctx, terms, rids)
	if err != nil {
		return nil, err
	}
	for _, rid := range rids {
		g := at[owner[rid]]
		g.Top = append(g.Top, hits[rid])
	}
	return out, nil
}

// SessionSearchHits returns a Session's Message hits for terms past its
// SearchTopHits best ones, in Transcript order: at most limit of them after
// skipping offset. total counts all its Message hits (hub.md §4.7).
func (s *Store) SessionSearchHits(ctx context.Context, sessionID int64, terms []string, offset, limit int) (hits []Hit, total int, err error) {
	if len(terms) == 0 {
		return nil, 0, nil
	}
	// The title row is rowid 0 of the Session's range; Messages follow it.
	match := []any{matchExpr(terms), searchKindMessage, searchRowID(sessionID, 1), searchRowID(sessionID, maxSearchRows), sessionID}
	const from = `
		FROM search JOIN sessions s ON s.id = ?5
		WHERE search MATCH ?1 AND search.kind = ?2 AND search.rowid BETWEEN ?3 AND ?4 AND s.parsed_at IS NOT NULL`
	if err := s.read.QueryRowContext(ctx, `SELECT count(*)`+from, match...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.read.QueryContext(ctx, `
		WITH h AS MATERIALIZED (SELECT search.rowid AS rid, bm25(search) AS score`+from+`)
		SELECT rid FROM (SELECT rid, row_number() OVER (ORDER BY score, rid) AS rn FROM h)
		WHERE rn > ?6
		ORDER BY rid
		LIMIT ?7 OFFSET ?8`,
		append(match, SearchTopHits, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	var rids []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rids = append(rids, rid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	byRow, err := s.hitsAt(ctx, terms, rids)
	if err != nil {
		return nil, 0, err
	}
	for _, rid := range rids {
		hits = append(hits, byRow[rid])
	}
	return hits, total, nil
}

// hitsAt returns the Message hits for terms at the given search rows, by
// rowid, with their snippets.
func (s *Store) hitsAt(ctx context.Context, terms []string, rids []int64) (map[int64]Hit, error) {
	out := map[int64]Hit{}
	if len(rids) == 0 {
		return out, nil
	}
	ridList, _ := json.Marshal(rids)
	rows, err := s.read.QueryContext(ctx, `
		SELECT search.rowid, search.session_id, coalesce(search.message_id, ''), coalesce(msg.role, ''),
			snippet(search, 0, ?, ?, '…', ?), coalesce(msg.timestamp, 0)
		FROM search
		LEFT JOIN messages msg ON msg.session_id = search.session_id AND msg.id = search.message_id
		WHERE search MATCH ? AND search.rowid IN (SELECT value FROM json_each(?))`,
		SnippetOpen, SnippetClose, snippetTokens, matchExpr(terms), string(ridList))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rid int64
			h   Hit
		)
		if err := rows.Scan(&rid, &h.SessionID, &h.MessageID, &h.Role, &h.Snippet, &h.Timestamp); err != nil {
			return nil, err
		}
		out[rid] = h
	}
	return out, rows.Err()
}
