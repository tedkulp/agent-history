package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/tedkulp/agent-history/protocol"
)

// reparseEnqueue queues the Sessions a SELECT of ids returns at re-parse
// priority, leaving any already-queued row alone (hub.md §3.8). It reports
// how many rows it added.
func reparseEnqueue(ctx context.Context, tx execer, now int64, selectIDs string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO parse_queue (session_id, priority, enqueued_at, not_before)
		SELECT id, 1, ?, ? FROM (`+selectIDs+`) WHERE true
		ON CONFLICT (session_id) DO NOTHING`, append([]any{now, now}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// hasMain selects Sessions with a main record: the only ones a parse can
// run on. Stubs and Sessions with only attachments so far are left out.
const hasMain = `EXISTS (SELECT 1 FROM raw_records r WHERE r.session_id = s.id AND r.role = 'main')`

// MapUnattached runs MapKey over every record that has no Session yet,
// attaches the ones that now map, and queues their Sessions at re-parse
// priority (hub.md §4.1 step 6). It returns how many records it attached.
func (s *Store) MapUnattached(ctx context.Context) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, machine_id, source, record_key FROM raw_records WHERE session_id IS NULL ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type rec struct {
		id                int64
		machine, src, key string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.machine, &r.src, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	t := s.now()
	attached := 0
	for _, r := range recs {
		m, ok := s.parsers.MapKey(r.src, r.key)
		if !ok {
			continue
		}
		sessionID, role, err := s.attachRecord(ctx, tx, r.id, r.machine, r.src, m)
		if err != nil {
			return 0, err
		}
		attached++
		if role == roleShadow {
			continue
		}
		if _, err := reparseEnqueue(ctx, tx, t, `SELECT ? AS id`, sessionID); err != nil {
			return 0, err
		}
	}
	return attached, tx.Commit()
}

// EnqueueStale queues at re-parse priority every Session whose
// parser_version differs from its Source's running parser (NULL included),
// except one that already failed at the running version (hub.md §4.5). It
// returns the count queued per Source.
func (s *Store) EnqueueStale(ctx context.Context) (map[string]int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := s.now()
	out := map[string]int64{}
	for _, src := range s.sources() {
		v := s.parsers[src].Version()
		n, err := reparseEnqueue(ctx, tx, t, `
			SELECT s.id FROM sessions s
			WHERE s.source = ? AND `+hasMain+`
				AND (s.parser_version IS NULL OR s.parser_version != ?)
				AND NOT (s.parse_status = 'failed' AND s.parse_attempted_version IS ?)`, src, v, v)
		if err != nil {
			return nil, err
		}
		out[src] = n
	}
	return out, tx.Commit()
}

// sources lists the Sources that have a parser, in order.
func (s *Store) sources() []string {
	var out []string
	for src := range s.parsers {
		out = append(out, src)
	}
	sort.Strings(out)
	return out
}

// ReparseScope picks the Sessions `agent-history-hub reparse` queues: all of
// them, one Source's, or one Session. The zero value means all.
type ReparseScope struct {
	Source    string
	SessionID int64
}

// ErrNoSession is returned by Reparse for a Session id that doesn't exist or
// has nothing to parse yet, such as a stub.
var ErrNoSession = errors.New("no such Session with a main Raw record")

// Reparse queues the Sessions in scope at re-parse priority, including ones
// that failed at the running parser version (hub.md §4.5). It returns how
// many it queued; Sessions already queued are not counted.
func (s *Store) Reparse(ctx context.Context, scope ReparseScope) (int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var (
		where = hasMain
		args  []any
	)
	switch {
	case scope.SessionID != 0:
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions s WHERE s.id = ? AND `+hasMain, scope.SessionID).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("%w: %d", ErrNoSession, scope.SessionID)
		}
		where += ` AND s.id = ?`
		args = append(args, scope.SessionID)
	case scope.Source != "":
		where += ` AND s.source = ?`
		args = append(args, scope.Source)
	}
	n, err := reparseEnqueue(ctx, tx, s.now(), `SELECT s.id FROM sessions s WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// QueuedReparses counts the Sessions queued at re-parse priority, for the home
// banner (hub.md §4.7).
func (s *Store) QueuedReparses(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM parse_queue WHERE priority = 1`).Scan(&n)
	return n, err
}

// Health counts a Machine's Sessions with Parse warnings and those whose
// parse failed (protocol.md §2.3).
func (s *Store) Health(ctx context.Context, machineID string) (protocol.Health, error) {
	var w, f sql.NullInt64
	err := s.read.QueryRowContext(ctx, `
		SELECT sum(`+hasParseWarnings+`), sum(s.parse_status = 'failed')
		FROM sessions s WHERE s.machine_id = ?`, machineID).Scan(&w, &f)
	return protocol.Health{SessionsWithWarnings: int(w.Int64), SessionsFailed: int(f.Int64)}, err
}
