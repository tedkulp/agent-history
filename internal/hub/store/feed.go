package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// FeedRow is one Session row of the home feed (hub.md §4.7).
type FeedRow struct {
	ID             int64
	Source         string
	NativeID       string
	Title          string
	FirstPrompt    string
	LastActivityAt int64
	ProjectCwd     string // "" = No project
	MachineID      string
	Machine        string // display label
	Failed         bool   // the latest parse failed
}

// machineLabel is display_name, else hostname, else the first 8 characters of
// the id (hub.md §3.2).
const machineLabel = `coalesce(nullif(m.display_name, ''), nullif(m.hostname, ''), substr(m.id, 1, 8))`

// feedVisible selects the Sessions the feed lists: top-level ones that parsed
// to at least one Message, or whose parse failed. A launch where nothing
// happened (say, only /model) parses to no Messages and would be a blank row;
// a failed one is listed even if it never parsed, so a parser bug can't hide
// it (hub.md §4.7).
const feedVisible = `s.parent_session_id IS NULL AND (s.parse_status = 'failed'
	OR s.parsed_at IS NOT NULL AND EXISTS (SELECT 1 FROM messages msg WHERE msg.session_id = s.id))`

// FeedFilter narrows the feed to the active chips (hub.md §4.7). Empty fields
// don't filter.
type FeedFilter struct {
	Machine  string
	Source   string
	Project  *string // nil = any; "" = No project
	Warnings bool    // only Sessions with Parse warnings or a failed parse
	Before   *Cursor // only rows older than this one (later in feed order)
}

// hasWarnings selects Sessions with any Parse warning or a failed parse: the
// "has warnings" chip (hub.md §4.7).
const hasWarnings = `(s.parse_status = 'failed' OR ` + hasParseWarnings + `)`

// hasParseWarnings selects Sessions with at least one Parse warning.
const hasParseWarnings = `EXISTS (SELECT 1 FROM parse_warnings w WHERE w.session_id = s.id)`

// Cursor is the position of a feed row: its last_activity_at, with the id
// breaking ties so paging neither skips nor repeats rows.
type Cursor struct {
	At int64
	ID int64
}

// Feed lists parsed top-level Sessions matching f, newest activity first, at
// most limit rows.
func (s *Store) Feed(ctx context.Context, f FeedFilter, limit int) ([]FeedRow, error) {
	where, args := chipWhere(f)
	where = append([]string{feedVisible}, where...)
	if f.Before != nil {
		where = append(where, `(coalesce(s.last_activity_at, 0), s.id) < (?, ?)`)
		args = append(args, f.Before.At, f.Before.ID)
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), coalesce(s.first_prompt, ''),
			coalesce(s.last_activity_at, 0), coalesce(s.project_cwd, ''), m.id, `+machineLabel+`,
			s.parse_status = 'failed'
		FROM sessions s JOIN machines m ON m.id = s.machine_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY coalesce(s.last_activity_at, 0) DESC, s.id DESC
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedRow
	for rows.Next() {
		var r FeedRow
		if err := rows.Scan(&r.ID, &r.Source, &r.NativeID, &r.Title, &r.FirstPrompt, &r.LastActivityAt, &r.ProjectCwd, &r.MachineID, &r.Machine, &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// chipWhere is the WHERE conditions and args for f's chips, on sessions s.
func chipWhere(f FeedFilter) ([]string, []any) {
	var (
		where []string
		args  []any
	)
	if f.Machine != "" {
		where = append(where, `s.machine_id = ?`)
		args = append(args, f.Machine)
	}
	if f.Source != "" {
		where = append(where, `s.source = ?`)
		args = append(args, f.Source)
	}
	if f.Project != nil {
		if *f.Project == "" {
			where = append(where, `s.project_cwd IS NULL`)
		} else {
			where = append(where, `s.project_cwd = ?`)
			args = append(args, *f.Project)
		}
	}
	if f.Warnings {
		where = append(where, hasWarnings)
	}
	return where, args
}

// MachineChip is one Machine chip of the feed.
type MachineChip struct {
	ID    string
	Label string
}

// FeedMachines lists the Machines that have Sessions in the feed, by label.
func (s *Store) FeedMachines(ctx context.Context) ([]MachineChip, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT m.id, `+machineLabel+` AS label FROM machines m
		WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.machine_id = m.id AND `+feedVisible+`)
		ORDER BY label, m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineChip
	for rows.Next() {
		var c MachineChip
		if err := rows.Scan(&c.ID, &c.Label); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FeedSources lists the Sources that have Sessions in the feed.
func (s *Store) FeedSources(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT s.source FROM sessions s WHERE `+feedVisible+` ORDER BY s.source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ProjectCount is one Project of a Machine with its feed Session count.
type ProjectCount struct {
	Cwd      string // "" = No project
	Sessions int
}

// MachineProjects lists a Machine's Projects by path, with "No project" last.
func (s *Store) MachineProjects(ctx context.Context, machineID string) ([]ProjectCount, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT coalesce(s.project_cwd, ''), count(*) FROM sessions s
		WHERE s.machine_id = ? AND `+feedVisible+`
		GROUP BY s.project_cwd
		ORDER BY s.project_cwd IS NULL, s.project_cwd`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectCount
	for rows.Next() {
		var p ProjectCount
		if err := rows.Scan(&p.Cwd, &p.Sessions); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// driftWindow is how recently a Session must have been active for its
// warnings to raise a drift banner (hub.md §4.7).
const driftWindow = 7 * 24 * time.Hour

// DriftBanner is one drift banner: Sessions of a Source version with Parse warnings.
type DriftBanner struct {
	Source        string
	SourceVersion string // "" when unknown
	Sessions      int
}

// DriftBanners lists each (source, source_version) with Parse warnings where
// at least one Session with warnings was active in the last 7 days. It counts
// top-level Sessions, the ones its "has warnings" feed link can show.
func (s *Store) DriftBanners(ctx context.Context) ([]DriftBanner, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT s.source, coalesce(s.source_version, ''), count(*)
		FROM sessions s
		WHERE s.parent_session_id IS NULL AND `+hasParseWarnings+`
		GROUP BY s.source, coalesce(s.source_version, '')
		HAVING max(coalesce(s.last_activity_at, 0)) >= ?
		ORDER BY s.source, coalesce(s.source_version, '')`, s.now()-driftWindow.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftBanner
	for rows.Next() {
		var d DriftBanner
		if err := rows.Scan(&d.Source, &d.SourceVersion, &d.Sessions); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ParseWarning is one aggregated Parse warning of a Session (hub.md §3.6).
type ParseWarning struct {
	Kind         string
	SourceType   string
	Count        int
	FirstExcerpt string
}

// SessionHeader is the Session data shown above a Transcript.
type SessionHeader struct {
	ID         int64
	Source     string
	NativeID   string
	Title      string
	MachineID  string
	Machine    string
	ProjectCwd string
	GitBranch  string
	Model      string
	StartedAt  int64
	// SourceVersion, Warnings and ParseError are for the Notes (hub.md
	// §4.7). ParseError is set when the latest parse failed; Parsed is
	// false when no parse ever succeeded, so there is no Transcript.
	SourceVersion string
	Warnings      []ParseWarning
	ParseError    string
	Parsed        bool
	// Parent is set for a Child Session; Children are the parsed Child
	// Sessions filed under it, oldest first (hub.md §4.7).
	Parent   *ParentLink
	Children []ChildLink
	// CallChildren maps each parsed Child Session named by this Session's
	// tool calls to its id. A nested Child Session is filed under the
	// top-level Session, so it's here but not in Children (claude-code.md §3.5).
	CallChildren map[string]int64
}

// ParentLink is a Child Session's parent and the spawning call in it.
type ParentLink struct {
	ID       int64
	NativeID string
	Title    string
	Parsed   bool   // false for a stub, which has no page
	CallPart string // the spawning tool_call Part's id; "" when not found
}

// ChildLink is one Child Session of a Session.
type ChildLink struct {
	ID       int64
	NativeID string
	Title    string
}

// TranscriptMessage is one Message with its Parts, in order.
type TranscriptMessage struct {
	ID        string
	Role      string
	Timestamp int64
	Parts     []TranscriptPart
}

// TranscriptPart is one Part with its raw payload JSON.
type TranscriptPart struct {
	ID      string
	Kind    string
	Payload string
}

// Transcript returns a Session's header and Messages. ok is false for an
// unknown id or a Session that was never parsed and hasn't failed, such as
// a stub (hub.md §4.10).
func (s *Store) Transcript(ctx context.Context, id int64) (h SessionHeader, msgs []TranscriptMessage, ok bool, err error) {
	if h, ok, err = s.sessionHeader(ctx, id); !ok || err != nil {
		return h, nil, ok, err
	}
	msgs, err = s.messagesFrom(ctx, id, 0)
	return h, msgs, err == nil, err
}

// ErrTranscriptChanged is returned by TranscriptTail when the Messages before
// the page's last one moved: a rewind, a new branch or a compaction.
var ErrTranscriptChanged = errors.New("transcript changed")

// TranscriptTail returns a Session's header and the Messages a page holding
// count Messages, the last with id after, needs to catch up: that last
// Message again, then every later one. With count 0 it returns every
// Message. It returns ErrTranscriptChanged when the Message at ordinal
// count-1 is no longer after (hub.md §4.7), and ok=false like Transcript.
func (s *Store) TranscriptTail(ctx context.Context, id int64, after string, count int) (h SessionHeader, msgs []TranscriptMessage, ok bool, err error) {
	if h, ok, err = s.sessionHeader(ctx, id); !ok || err != nil {
		return h, nil, ok, err
	}
	from := max(count-1, 0)
	// One query, so the check and the Messages come from one snapshot. The
	// header is read apart; a stale one is fixed by the next update.
	if msgs, err = s.messagesFrom(ctx, id, from); err != nil {
		return h, nil, false, err
	}
	if count > 0 && (len(msgs) == 0 || msgs[0].ID != after) {
		return h, nil, true, ErrTranscriptChanged
	}
	return h, msgs, true, nil
}

// ParentSession returns a Session's parent id, or ok=false for a top-level
// Session.
func (s *Store) ParentSession(ctx context.Context, id int64) (parent int64, ok bool, err error) {
	var p sql.NullInt64
	err = s.read.QueryRowContext(ctx, `SELECT parent_session_id FROM sessions WHERE id = ?`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return p.Int64, p.Valid, err
}

// HasTranscript reports whether a Session has a Transcript page: it exists
// and was parsed or failed, so it isn't a stub.
func (s *Store) HasTranscript(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := s.read.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ? AND (parsed_at IS NOT NULL OR parse_status = 'failed'))`, id).Scan(&ok)
	return ok, err
}

// sessionHeader loads the Session data shown above a Transcript, with ok
// false like Transcript.
func (s *Store) sessionHeader(ctx context.Context, id int64) (h SessionHeader, ok bool, err error) {
	var (
		parentID       sql.NullInt64
		spawningCallID string
		parseErr       sql.NullString
	)
	err = s.read.QueryRowContext(ctx, `
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), m.id, `+machineLabel+`, coalesce(s.project_cwd, ''),
			coalesce(s.git_branch, ''), coalesce(s.model, ''), coalesce(s.started_at, 0), coalesce(s.source_version, ''),
			s.parent_session_id, coalesce(s.spawning_call_id, ''),
			CASE WHEN s.parse_status = 'failed' THEN coalesce(s.parse_error, '') END, s.parsed_at IS NOT NULL
		FROM sessions s JOIN machines m ON m.id = s.machine_id
		WHERE s.id = ? AND (s.parsed_at IS NOT NULL OR s.parse_status = 'failed')`, id).Scan(
		&h.ID, &h.Source, &h.NativeID, &h.Title, &h.MachineID, &h.Machine, &h.ProjectCwd, &h.GitBranch, &h.Model, &h.StartedAt, &h.SourceVersion,
		&parentID, &spawningCallID, &parseErr, &h.Parsed)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	if parseErr.Valid {
		h.ParseError = parseErr.String
		if h.ParseError == "" {
			h.ParseError = "unknown error"
		}
	}
	if h.Warnings, err = s.parseWarnings(ctx, id); err != nil {
		return h, false, err
	}
	if parentID.Valid {
		if h.Parent, err = s.parentLink(ctx, parentID.Int64, spawningCallID, h.NativeID); err != nil {
			return h, false, err
		}
	}
	if h.Children, err = s.childLinks(ctx, id); err != nil {
		return h, false, err
	}
	if h.CallChildren, err = s.callChildren(ctx, id); err != nil {
		return h, false, err
	}
	return h, true, nil
}

// messagesFrom loads a Session's Messages from ordinal from on, with their
// Parts, in order.
func (s *Store) messagesFrom(ctx context.Context, id int64, from int) ([]TranscriptMessage, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT m.id, m.role, coalesce(m.timestamp, 0), p.id, p.kind, p.payload_json
		FROM messages m LEFT JOIN parts p ON p.session_id = m.session_id AND p.message_id = m.id
		WHERE m.session_id = ? AND m.ordinal >= ?
		ORDER BY m.ordinal, p.ordinal`, id, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []TranscriptMessage
	for rows.Next() {
		var (
			mid, role          string
			ts                 int64
			pid, kind, payload sql.NullString
		)
		if err := rows.Scan(&mid, &role, &ts, &pid, &kind, &payload); err != nil {
			return nil, err
		}
		if len(msgs) == 0 || msgs[len(msgs)-1].ID != mid {
			msgs = append(msgs, TranscriptMessage{ID: mid, Role: role, Timestamp: ts})
		}
		if pid.Valid {
			m := &msgs[len(msgs)-1]
			m.Parts = append(m.Parts, TranscriptPart{ID: pid.String, Kind: kind.String, Payload: payload.String})
		}
	}
	return msgs, rows.Err()
}

// parentLink finds a Child Session's parent and the call that spawned it:
// the tool_call Part with the child's spawning call id, else one whose
// child_sessions names the child (hub.md §4.7).
func (s *Store) parentLink(ctx context.Context, parentID int64, spawningCallID, childNativeID string) (*ParentLink, error) {
	p := &ParentLink{ID: parentID}
	if err := s.read.QueryRowContext(ctx, `
		SELECT native_id, coalesce(title, ''), parsed_at IS NOT NULL FROM sessions WHERE id = ?`, parentID).Scan(
		&p.NativeID, &p.Title, &p.Parsed); err != nil {
		return nil, err
	}
	if !p.Parsed {
		return p, nil
	}
	err := s.read.QueryRowContext(ctx, `
		SELECT p.id FROM parts p JOIN messages m ON m.session_id = p.session_id AND m.id = p.message_id
		WHERE p.session_id = ?1 AND p.kind = ?2 AND (
			(?3 != '' AND p.payload_json ->> '$.call_id' = ?3)
			OR EXISTS (SELECT 1 FROM json_each(p.payload_json, '$.child_sessions') WHERE value = ?4))
		ORDER BY p.payload_json ->> '$.call_id' = ?3 DESC, m.ordinal, p.ordinal
		LIMIT 1`, parentID, parser.KindToolCall, spawningCallID, childNativeID).Scan(&p.CallPart)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return p, nil
}

// childLinks lists a Session's parsed Child Sessions, oldest first.
func (s *Store) childLinks(ctx context.Context, id int64) ([]ChildLink, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT id, native_id, coalesce(title, '') FROM sessions
		WHERE parent_session_id = ? AND parsed_at IS NOT NULL
		ORDER BY started_at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChildLink
	for rows.Next() {
		var c ChildLink
		if err := rows.Scan(&c.ID, &c.NativeID, &c.Title); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// callChildren maps the native id of each parsed Child Session named in a
// Session's tool_call Parts to its id.
func (s *Store) callChildren(ctx context.Context, id int64) (map[string]int64, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT DISTINCT c.native_id, c.id
		FROM sessions s
		JOIN parts p ON p.session_id = s.id AND p.kind = ?2
		JOIN json_each(p.payload_json, '$.child_sessions') j
		JOIN sessions c ON c.machine_id = s.machine_id AND c.source = s.source AND c.native_id = j.value
		WHERE s.id = ?1 AND c.parsed_at IS NOT NULL`, id, parser.KindToolCall)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var native string
		var cid int64
		if err := rows.Scan(&native, &cid); err != nil {
			return nil, err
		}
		out[native] = cid
	}
	return out, rows.Err()
}

// parseWarnings lists a Session's Parse warnings, most frequent first.
func (s *Store) parseWarnings(ctx context.Context, sessionID int64) ([]ParseWarning, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT kind, source_type, count, coalesce(first_excerpt, '') FROM parse_warnings
		WHERE session_id = ? ORDER BY count DESC, kind, source_type`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParseWarning
	for rows.Next() {
		var w ParseWarning
		if err := rows.Scan(&w.Kind, &w.SourceType, &w.Count, &w.FirstExcerpt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ToolOutput returns the full output of one tool_call Part: from
// tool_outputs when it was split out, else the payload's inline output
// (hub.md §4.7). ok is false for an unknown Part or one that isn't a tool call.
func (s *Store) ToolOutput(ctx context.Context, sessionID int64, partID string) (out string, ok bool, err error) {
	var b []byte
	err = s.read.QueryRowContext(ctx, `
		SELECT bytes FROM tool_outputs WHERE session_id = ? AND part_id = ?`, sessionID, partID).Scan(&b)
	if err == nil {
		return string(b), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var payload string
	err = s.read.QueryRowContext(ctx, `
		SELECT payload_json FROM parts WHERE session_id = ? AND id = ? AND kind = ?`,
		sessionID, partID, parser.KindToolCall).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var tc parser.ToolCallPayload
	if err := json.Unmarshal([]byte(payload), &tc); err != nil {
		return "", false, err
	}
	if tc.Output == nil {
		return "", true, nil
	}
	return *tc.Output, true, nil
}

// Blob returns an image blob by its SHA-256. ok is false when there is none.
func (s *Store) Blob(ctx context.Context, sha256 string) (mime string, b []byte, ok bool, err error) {
	err = s.read.QueryRowContext(ctx, `SELECT mime, bytes FROM blobs WHERE sha256 = ?`, sha256).Scan(&mime, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	return mime, b, err == nil, err
}
