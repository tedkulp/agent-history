package store

import (
	"context"
	"database/sql"
	"errors"
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
	Machine        string // display label
}

// machineLabel is display_name, else hostname, else the first 8 characters of
// the id (hub.md §3.2).
const machineLabel = `coalesce(nullif(m.display_name, ''), nullif(m.hostname, ''), substr(m.id, 1, 8))`

// Feed lists parsed top-level Sessions, newest activity first.
func (s *Store) Feed(ctx context.Context, limit int) ([]FeedRow, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), coalesce(s.first_prompt, ''),
			coalesce(s.last_activity_at, 0), coalesce(s.project_cwd, ''), `+machineLabel+`
		FROM sessions s JOIN machines m ON m.id = s.machine_id
		WHERE s.parent_session_id IS NULL AND s.parsed_at IS NOT NULL
		ORDER BY s.last_activity_at DESC, s.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedRow
	for rows.Next() {
		var r FeedRow
		if err := rows.Scan(&r.ID, &r.Source, &r.NativeID, &r.Title, &r.FirstPrompt, &r.LastActivityAt, &r.ProjectCwd, &r.Machine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionHeader is the Session data shown above a Transcript.
type SessionHeader struct {
	ID         int64
	Source     string
	NativeID   string
	Title      string
	Machine    string
	ProjectCwd string
	GitBranch  string
	Model      string
	StartedAt  int64
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
// unknown id or a Session that was never parsed (hub.md §4.10).
func (s *Store) Transcript(ctx context.Context, id int64) (h SessionHeader, msgs []TranscriptMessage, ok bool, err error) {
	err = s.read.QueryRowContext(ctx, `
		SELECT s.id, s.source, s.native_id, coalesce(s.title, ''), `+machineLabel+`, coalesce(s.project_cwd, ''),
			coalesce(s.git_branch, ''), coalesce(s.model, ''), coalesce(s.started_at, 0)
		FROM sessions s JOIN machines m ON m.id = s.machine_id
		WHERE s.id = ? AND s.parsed_at IS NOT NULL`, id).Scan(
		&h.ID, &h.Source, &h.NativeID, &h.Title, &h.Machine, &h.ProjectCwd, &h.GitBranch, &h.Model, &h.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, nil, false, nil
	}
	if err != nil {
		return h, nil, false, err
	}

	rows, err := s.read.QueryContext(ctx, `
		SELECT m.id, m.role, coalesce(m.timestamp, 0), p.id, p.kind, p.payload_json
		FROM messages m LEFT JOIN parts p ON p.session_id = m.session_id AND p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.ordinal, p.ordinal`, id)
	if err != nil {
		return h, nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			mid, role          string
			ts                 int64
			pid, kind, payload sql.NullString
		)
		if err := rows.Scan(&mid, &role, &ts, &pid, &kind, &payload); err != nil {
			return h, nil, false, err
		}
		if len(msgs) == 0 || msgs[len(msgs)-1].ID != mid {
			msgs = append(msgs, TranscriptMessage{ID: mid, Role: role, Timestamp: ts})
		}
		if pid.Valid {
			m := &msgs[len(msgs)-1]
			m.Parts = append(m.Parts, TranscriptPart{ID: pid.String, Kind: kind.String, Payload: payload.String})
		}
	}
	return h, msgs, true, rows.Err()
}
