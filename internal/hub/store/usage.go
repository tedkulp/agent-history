package store

import (
	"context"
	"fmt"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// Usage breakdowns (hub.md §4.7).
const (
	UsageBySource  = "source"
	UsageByMachine = "machine"
	UsageByProject = "project"
	UsageByModel   = "model"
)

// UsageCounts is the Usage in a window with its Session and Message counts.
// Sessions counts top-level Sessions only, as the feed does; Messages counts
// every Message.
type UsageCounts struct {
	parser.Usage
	Sessions int
	Messages int
}

// dest is where a row of usageSums scans to.
func (c *UsageCounts) dest() []any {
	return []any{&c.Input, &c.Output, &c.CacheRead, &c.CacheWrite, &c.Reasoning, &c.Sessions, &c.Messages}
}

// UsageGroup is one row of a Usage breakdown.
type UsageGroup struct {
	Key     string // the Source, Machine id, Machine id then Project cwd, or model
	Label   string // the Source, Machine label, Project cwd or model
	Machine string // the Project's Machine label; "" in other breakdowns
	UsageCounts
}

// UsageDay is the Usage dated on one day.
type UsageDay struct {
	Day time.Time // midnight in the report's zone
	parser.Usage
}

// UsageGroupDay is each breakdown group's tokens (all five fields added) on
// one day, by UsageGroup.Key.
type UsageGroupDay struct {
	Day    time.Time
	Tokens map[string]int64
}

// UsageReport is the Usage page's data (hub.md §4.7).
type UsageReport struct {
	Total     UsageCounts
	NoUsage   int             // top-level Sessions in the window that reported no Usage
	Days      []UsageDay      // the days with Usage, oldest first
	Groups    []UsageGroup    // most tokens first
	GroupDays []UsageGroupDay // the days with Usage, oldest first
}

// usageWindow lists, from ?1 on, each Message (msg = 1) dated by its
// timestamp, else its Session's start, plus the Usage of each Session that
// has it with no per-Message breakdown (msg = 0), dated by the Session's
// start. root is the top-level Session it belongs to.
const usageWindow = `
WITH RECURSIVE roots(id, root) AS (
	SELECT id, id FROM sessions WHERE parent_session_id IS NULL
	UNION ALL
	SELECT s.id, r.root FROM sessions s JOIN roots r ON s.parent_session_id = r.id
),
items(sid, at, model, u, msg) AS (
	SELECT m.session_id, coalesce(m.timestamp, s.started_at),
		coalesce(nullif(m.model, ''), nullif(s.model, ''), 'unknown'), m.usage_json, 1
	FROM messages m JOIN sessions s ON s.id = m.session_id
	UNION ALL
	SELECT s.id, s.started_at, coalesce(nullif(s.model, ''), 'unknown'), s.usage_json, 0
	FROM sessions s
	WHERE s.usage_json IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND m.usage_json IS NOT NULL)
),
win AS (
	SELECT w.*, s.source, s.machine_id, s.parent_session_id, coalesce(r.root, w.sid) AS root
	FROM items w JOIN sessions s ON s.id = w.sid LEFT JOIN roots r ON r.id = w.sid
	WHERE w.at >= ?1
)`

// usageSums are the five Usage sums, then the Session and Message counts.
const usageSums = `
	coalesce(sum(json_extract(win.u, '$.input')), 0) AS input,
	coalesce(sum(json_extract(win.u, '$.output')), 0) AS output,
	coalesce(sum(json_extract(win.u, '$.cache_read')), 0) AS cache_read,
	coalesce(sum(json_extract(win.u, '$.cache_write')), 0) AS cache_write,
	coalesce(sum(json_extract(win.u, '$.reasoning')), 0) AS reasoning,
	count(DISTINCT CASE WHEN win.msg = 1 AND win.parent_session_id IS NULL THEN win.sid END),
	coalesce(sum(win.msg), 0)`

// usageTokens is the five Usage fields of a win row added together.
const usageTokens = `coalesce(json_extract(win.u, '$.input'), 0) + coalesce(json_extract(win.u, '$.output'), 0) +
	coalesce(json_extract(win.u, '$.cache_read'), 0) + coalesce(json_extract(win.u, '$.cache_write'), 0) +
	coalesce(json_extract(win.u, '$.reasoning'), 0)`

// usageGrouping is a breakdown's key, label and Machine columns and the
// joins they need. The key is also its GROUP BY.
type usageGrouping struct{ key, label, machine, join string }

// usageGroupings are the breakdowns. A Project's key is its Machine id then
// its cwd, since the same path on two Machines is two Projects; a Child
// Session's Project is its top-level Session's.
var usageGroupings = map[string]usageGrouping{
	UsageBySource:  {`win.source`, `win.source`, `''`, ``},
	UsageByMachine: {`m.id`, machineLabel, `''`, `JOIN machines m ON m.id = win.machine_id`},
	UsageByProject: {`rs.machine_id || coalesce(rs.project_cwd, '')`, `coalesce(rs.project_cwd, '')`, machineLabel,
		`JOIN sessions rs ON rs.id = win.root JOIN machines m ON m.id = rs.machine_id`},
	UsageByModel: {`win.model`, `win.model`, `''`, ``},
}

// UsageReport totals Usage dated from since (Unix ms; 0 = all time) on, by
// day in loc and in the breakdown by (hub.md §4.7).
func (s *Store) UsageReport(ctx context.Context, since int64, loc *time.Location, by string) (UsageReport, error) {
	g, ok := usageGroupings[by]
	if !ok {
		return UsageReport{}, fmt.Errorf("unknown Usage breakdown %q", by)
	}
	var rep UsageReport
	if err := s.read.QueryRowContext(ctx, usageWindow+`SELECT `+usageSums+` FROM win`, since).Scan(rep.Total.dest()...); err != nil {
		return UsageReport{}, err
	}
	if err := s.read.QueryRowContext(ctx, `
		SELECT count(*) FROM sessions s
		WHERE s.parent_session_id IS NULL AND s.usage_json IS NULL
			AND EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND coalesce(m.timestamp, s.started_at) >= ?)`,
		since).Scan(&rep.NoUsage); err != nil {
		return UsageReport{}, err
	}
	var err error
	if rep.Days, err = s.usageDays(ctx, since, loc); err != nil {
		return UsageReport{}, err
	}
	if rep.GroupDays, err = s.usageGroupDays(ctx, since, loc, g); err != nil {
		return UsageReport{}, err
	}

	rows, err := s.read.QueryContext(ctx, usageWindow+`
		SELECT `+g.key+`, `+g.label+`, `+g.machine+`, `+usageSums+`
		FROM win `+g.join+`
		GROUP BY 1
		ORDER BY input + output + cache_read + cache_write + reasoning DESC, 2`, since)
	if err != nil {
		return UsageReport{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r UsageGroup
		if err := rows.Scan(append([]any{&r.Key, &r.Label, &r.Machine}, r.dest()...)...); err != nil {
			return UsageReport{}, err
		}
		rep.Groups = append(rep.Groups, r)
	}
	return rep, rows.Err()
}

// usageBucket is how finely usageDays sums in SQL before Go dates each sum
// by day: 15 minutes, since every zone's offset is a whole number of them,
// so no bucket straddles a local midnight.
const usageBucket = 15 * time.Minute

// usageDays sums the dated Usage by day in loc, oldest first.
func (s *Store) usageDays(ctx context.Context, since int64, loc *time.Location) ([]UsageDay, error) {
	rows, err := s.read.QueryContext(ctx, usageWindow+`
		SELECT win.at / ?2, `+usageSums+`
		FROM win WHERE win.u IS NOT NULL
		GROUP BY 1 ORDER BY 1`, since, usageBucket.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []UsageDay
	for rows.Next() {
		var (
			bucket int64
			c      UsageCounts
		)
		if err := rows.Scan(append([]any{&bucket}, c.dest()...)...); err != nil {
			return nil, err
		}
		day := bucketDay(bucket, loc)
		if len(days) == 0 || !days[len(days)-1].Day.Equal(day) {
			days = append(days, UsageDay{Day: day})
		}
		days[len(days)-1].Add(c.Usage)
	}
	return days, rows.Err()
}

// usageGroupDays sums the dated tokens by day in loc and group g, oldest
// first.
func (s *Store) usageGroupDays(ctx context.Context, since int64, loc *time.Location, g usageGrouping) ([]UsageGroupDay, error) {
	rows, err := s.read.QueryContext(ctx, usageWindow+`
		SELECT win.at / ?2, `+g.key+`, sum(`+usageTokens+`)
		FROM win `+g.join+` WHERE win.u IS NOT NULL
		GROUP BY 1, 2 ORDER BY 1`, since, usageBucket.Milliseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []UsageGroupDay
	for rows.Next() {
		var (
			bucket, tokens int64
			key            string
		)
		if err := rows.Scan(&bucket, &key, &tokens); err != nil {
			return nil, err
		}
		day := bucketDay(bucket, loc)
		if len(days) == 0 || !days[len(days)-1].Day.Equal(day) {
			days = append(days, UsageGroupDay{Day: day, Tokens: map[string]int64{}})
		}
		days[len(days)-1].Tokens[key] += tokens
	}
	return days, rows.Err()
}

// bucketDay is the local midnight starting the day a usageBucket falls on.
func bucketDay(bucket int64, loc *time.Location) time.Time {
	y, m, d := time.UnixMilli(bucket * usageBucket.Milliseconds()).In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
