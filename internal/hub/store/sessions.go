package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// parseInterval is the least time between two parses of one Session (hub.md §3.8).
const parseInterval = 10 * time.Second

type queryer interface {
	execer
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sessionFor returns the id of the (machine, source, native id) Session,
// creating a pending row if there is none (hub.md §3.4).
func sessionFor(ctx context.Context, tx queryer, machineID, source, nativeID string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (machine_id, source, native_id) VALUES (?, ?, ?)
		ON CONFLICT (machine_id, source, native_id) DO NOTHING`, machineID, source, nativeID); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM sessions WHERE machine_id = ? AND source = ? AND native_id = ?`,
		machineID, source, nativeID).Scan(&id)
	return id, err
}

// attachRecord links a new Raw record to its Session through its MapKey
// result (hub.md §4.2 step 2) and returns the Session id.
func attachRecord(ctx context.Context, tx queryer, recordID int64, machineID, source string, m parser.Mapping) (int64, error) {
	sessionID, err := sessionFor(ctx, tx, machineID, source, m.NativeID)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE raw_records SET session_id = ?, role = ?, layout = ?, layout_rank = ? WHERE id = ?`,
		sessionID, m.Role, m.Layout, m.LayoutRank, recordID)
	return sessionID, err
}

// liveEnqueue queues a Session at live priority, no sooner than 10 s after its
// last parse (hub.md §3.8). On conflict it keeps not_before. enqueued_at always
// moves forward, so the worker notices data that arrived during a parse even
// within the same millisecond.
func liveEnqueue(ctx context.Context, tx execer, sessionID, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before)
		SELECT id, 0, ?1, max(?1, coalesce(parsed_at + ?2, 0)) FROM sessions WHERE id = ?3
		ON CONFLICT (session_id) DO UPDATE SET
			priority = 0,
			enqueued_at = max(excluded.enqueued_at, parse_queue.enqueued_at + 1)`,
		now, parseInterval.Milliseconds(), sessionID)
	return err
}

// ProjectCwd returns the project_cwd for a Session's starting cwd on a
// Machine with the given home dir, or "" for "No project" (hub.md §4.4).
func ProjectCwd(cwd, homeDir string) string {
	if cwd == "" {
		return ""
	}
	cwd = path.Clean(cwd)
	if homeDir != "" && cwd == path.Clean(homeDir) {
		return ""
	}
	switch cwd {
	case "/tmp", "/private/tmp", "/var/tmp":
		return ""
	}
	for _, p := range []string{"/tmp/", "/private/tmp/", "/var/tmp/", "/var/folders/", "/private/var/folders/"} {
		if strings.HasPrefix(cwd, p) {
			return ""
		}
	}
	return cwd
}

// reassignProjects recomputes project_cwd for every Session of a Machine.
func reassignProjects(ctx context.Context, tx *sql.Tx, machineID, homeDir string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, cwd FROM sessions WHERE machine_id = ? AND cwd IS NOT NULL`, machineID)
	if err != nil {
		return err
	}
	type session struct {
		id  int64
		cwd string
	}
	var ss []session
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.id, &s.cwd); err != nil {
			rows.Close()
			return err
		}
		ss = append(ss, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range ss {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET project_cwd = ? WHERE id = ?`,
			nullStr(ProjectCwd(s.cwd, homeDir)), s.id); err != nil {
			return err
		}
	}
	return nil
}

// Job is one parse_queue row picked by the worker.
type Job struct {
	SessionID  int64
	EnqueuedAt int64
}

// NextJob returns the next due job (hub.md §4.5), or ok=false with the time
// of the earliest not_before (0 when the queue is empty).
func (s *Store) NextJob(ctx context.Context) (job Job, ok bool, nextAt int64, err error) {
	err = s.read.QueryRowContext(ctx, `
		SELECT q.session_id, q.enqueued_at
		FROM parse_queue q JOIN sessions s ON s.id = q.session_id
		WHERE q.not_before <= ?
		ORDER BY q.priority,
			CASE WHEN q.priority = 0 THEN q.not_before END,
			s.last_activity_at DESC NULLS LAST
		LIMIT 1`, s.now()).Scan(&job.SessionID, &job.EnqueuedAt)
	if err == nil {
		return job, true, 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, 0, err
	}
	var next sql.NullInt64
	err = s.read.QueryRowContext(ctx, `SELECT min(not_before) FROM parse_queue`).Scan(&next)
	return Job{}, false, next.Int64, err
}

// ParseInput is what the worker hands a parser for one job.
type ParseInput struct {
	Source string
	Input  parser.Input
}

// LoadParseInput loads a Session's current main and attachment records from
// its winning Layout (hub.md §4.3, §4.5 step 1). ok is false when the Session
// has no main record yet.
func (s *Store) LoadParseInput(ctx context.Context, sessionID int64) (in ParseInput, ok bool, err error) {
	var homeDir sql.NullString
	err = s.read.QueryRowContext(ctx, `
		SELECT s.source, s.native_id, m.home_dir
		FROM sessions s JOIN machines m ON m.id = s.machine_id
		WHERE s.id = ?`, sessionID).Scan(&in.Source, &in.Input.NativeID, &homeDir)
	if err != nil {
		return in, false, err
	}
	in.Input.HomeDir = homeDir.String

	// The winning Layout is the highest-ranked one holding a main; within it
	// the main with the highest id is parsed.
	var mainID int64
	var layout string
	err = s.read.QueryRowContext(ctx, `
		SELECT id, record_key, layout FROM raw_records
		WHERE session_id = ? AND role = ?
		ORDER BY layout_rank DESC, id DESC LIMIT 1`, sessionID, parser.RoleMain).Scan(&mainID, &in.Input.MainKey, &layout)
	if errors.Is(err, sql.ErrNoRows) {
		return in, false, nil
	}
	if err != nil {
		return in, false, err
	}
	if in.Input.Main, err = s.recordContent(ctx, mainID); err != nil {
		return in, false, err
	}

	rows, err := s.read.QueryContext(ctx, `
		SELECT id, record_key FROM raw_records
		WHERE session_id = ? AND role = ? AND layout = ?
		ORDER BY id`, sessionID, parser.RoleAttachment, layout)
	if err != nil {
		return in, false, err
	}
	type att struct {
		id  int64
		key string
	}
	var atts []att
	for rows.Next() {
		var a att
		if err := rows.Scan(&a.id, &a.key); err != nil {
			rows.Close()
			return in, false, err
		}
		atts = append(atts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, false, err
	}
	in.Input.Attachments = map[string][]byte{}
	for _, a := range atts {
		b, err := s.recordContent(ctx, a.id)
		if err != nil {
			return in, false, err
		}
		in.Input.Attachments[a.key] = b
	}
	return in, true, nil
}

// DropJob deletes a job whose Session has nothing to parse yet, unless new
// data arrived since it was picked.
func (s *Store) DropJob(ctx context.Context, job Job) error {
	_, err := deleteJob(ctx, s.write, job)
	return err
}

// deleteJob deletes the job's queue row if its enqueued_at is unchanged, and
// reports whether it did.
func deleteJob(ctx context.Context, db execer, job Job) (bool, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM parse_queue WHERE session_id = ? AND enqueued_at = ?`, job.SessionID, job.EnqueuedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// finishJob deletes the queue row if nothing arrived since the job was
// picked, else pushes it back by the parse interval (hub.md §4.5 step 3).
func finishJob(ctx context.Context, tx execer, job Job, now int64) error {
	deleted, err := deleteJob(ctx, tx, job)
	if err != nil || deleted {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE parse_queue SET not_before = ? WHERE session_id = ?`, now+parseInterval.Milliseconds(), job.SessionID)
	return err
}

// SaveParse writes a successful parse in one transaction (hub.md §4.5 step 3).
func (s *Store) SaveParse(ctx context.Context, job Job, parserVersion int, res parser.Result) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := s.now()
	id := job.SessionID

	var machineID, source string
	var homeDir sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT s.machine_id, s.source, m.home_dir
		FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).Scan(&machineID, &source, &homeDir); err != nil {
		return err
	}

	for _, q := range []string{
		`DELETE FROM parts WHERE session_id = ?`, `DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM tool_outputs WHERE session_id = ?`, `DELETE FROM parse_warnings WHERE session_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}

	var (
		firstPrompt string
		model       string
		total       parser.Usage
		hasUsage    bool
	)
	for i, m := range res.Messages {
		var usage sql.NullString
		if m.Usage != nil {
			b, _ := json.Marshal(m.Usage)
			usage = sql.NullString{String: string(b), Valid: true}
			total.Add(*m.Usage)
			hasUsage = true
		}
		if m.Role == parser.MessageAssistant && m.Model != "" {
			model = m.Model
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages (session_id, id, ordinal, role, timestamp, model, provider, usage_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, m.ID, i, m.Role, nullInt(m.Timestamp), nullStr(m.Model), nullStr(m.Provider), usage); err != nil {
			return err
		}
		var texts []string
		for j, p := range m.Parts {
			if tc, ok := p.Payload.(parser.ToolCallPayload); ok {
				if p.Payload, err = splitOutput(ctx, tx, id, p.ID, tc); err != nil {
					return err
				}
			}
			payload, err := json.Marshal(p.Payload)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO parts (session_id, message_id, id, ordinal, kind, payload_json) VALUES (?, ?, ?, ?, ?, ?)`,
				id, m.ID, p.ID, j, p.Kind, string(payload)); err != nil {
				return err
			}
			if tp, ok := p.Payload.(parser.TextPayload); ok {
				texts = append(texts, tp.Text)
			}
		}
		if firstPrompt == "" && m.Role == parser.MessageUser && len(texts) > 0 {
			firstPrompt = truncate(strings.Join(texts, "\n"), 300)
		}
	}

	for _, img := range res.Images {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO blobs (sha256, mime, bytes) VALUES (?, ?, ?)`, img.SHA256, img.MIME, img.Bytes); err != nil {
			return err
		}
	}

	for _, w := range res.Warnings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO parse_warnings (session_id, kind, source_type, count, first_excerpt) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (session_id, kind, source_type) DO UPDATE SET count = count + excluded.count`,
			id, w.Kind, w.SourceType, w.Count, nullStr(w.FirstExcerpt)); err != nil {
			return err
		}
	}

	title := res.Session.Title
	if title == "" {
		title = truncate(firstPrompt, 80)
	}
	var usageJSON sql.NullString
	if hasUsage {
		b, _ := json.Marshal(total)
		usageJSON = sql.NullString{String: string(b), Valid: true}
	}
	var parentID sql.NullInt64
	if res.Session.ParentNativeID != "" {
		pid, err := sessionFor(ctx, tx, machineID, source, res.Session.ParentNativeID)
		if err != nil {
			return err
		}
		parentID = sql.NullInt64{Int64: pid, Valid: true}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET
			title = ?, first_prompt = ?, started_at = ?, last_activity_at = ?, cwd = ?, project_cwd = ?,
			git_branch = ?, source_version = ?, model = ?, parent_session_id = ?, spawning_call_id = ?,
			usage_json = ?, parse_status = 'ok', parse_error = NULL, parsed_at = ?,
			parser_version = ?, parse_attempted_version = ?
		WHERE id = ?`,
		nullStr(title), nullStr(firstPrompt), nullInt(res.Session.StartedAt), nullInt(res.Session.LastActivityAt),
		nullStr(res.Session.Cwd), nullStr(ProjectCwd(res.Session.Cwd, homeDir.String)),
		nullStr(res.Session.GitBranch), nullStr(res.Session.SourceVersion), nullStr(model), parentID,
		nullStr(res.Session.SpawningCallID), usageJSON, t, parserVersion, parserVersion, id); err != nil {
		return err
	}
	if err := finishJob(ctx, tx, job, t); err != nil {
		return err
	}
	return tx.Commit()
}

// Tool output thresholds (hub.md §3.5).
const (
	previewOver  = 4 << 10  // output over this gets output_preview
	splitOver    = 16 << 10 // output over this goes to tool_outputs
	previewChars = 200
)

// splitOutput sets a tool call's output_size and output_preview, and moves
// output over 16 KB into tool_outputs (hub.md §3.5).
func splitOutput(ctx context.Context, tx execer, sessionID int64, partID string, tc parser.ToolCallPayload) (parser.ToolCallPayload, error) {
	if tc.Output == nil {
		return tc, nil
	}
	out := *tc.Output
	tc.OutputSize = len(out)
	if tc.OutputSize > previewOver {
		tc.OutputPreview = truncate(out, previewChars)
	}
	if tc.OutputSize > splitOver {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_outputs (session_id, part_id, bytes) VALUES (?, ?, ?)`, sessionID, partID, []byte(out)); err != nil {
			return tc, err
		}
		tc.Output = nil
	}
	return tc, nil
}

// SaveFailure records a parse failure, keeping the previous Transcript
// (hub.md §4.5 step 3).
func (s *Store) SaveFailure(ctx context.Context, job Job, parserVersion int, parseErr string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := s.now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET parse_status = 'failed', parse_error = ?, parse_attempted_version = ?,
			last_activity_at = coalesce(last_activity_at, ?)
		WHERE id = ?`, parseErr, parserVersion, t, job.SessionID); err != nil {
		return err
	}
	if err := finishJob(ctx, tx, job, t); err != nil {
		return err
	}
	return tx.Commit()
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: i != 0} }
