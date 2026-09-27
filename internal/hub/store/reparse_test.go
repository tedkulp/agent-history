package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/protocol"
)

// bumped is the claude-code parser with its parser_version raised by one, as
// a new Hub image would have it.
type bumped struct{ *claudecode.Parser }

func (b bumped) Version() int { return b.Parser.Version() + 1 }

func openDir(t *testing.T, dir string, reg parser.Registry) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &fakeClock{t: time.UnixMilli(1_800_000_000_000)}
	s.SetClock(c.now)
	return s
}

func uuid(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }

func claudeKey(native string) string { return "-Users-ted-src-app/" + native + ".jsonl" }

func TestParserBumpRequeuesStaleSessionsAtReparsePriority(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cur := claudecode.New()

	s := openDir(t, dir, parser.NewRegistry(cur))
	if err := s.UpsertMachine(ctx, "m1", protocol.MachineInfo{HomeDir: "/Users/ted"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{uuid(1), uuid(2), uuid(3)} {
		appendTo(t, s, protocol.SourceClaudeCode, claudeKey(n), []byte(line1))
	}
	for parseNext(t, s) {
	}
	a, b, failed := sessionIDOf(t, s, uuid(1)), sessionIDOf(t, s, uuid(2)), sessionIDOf(t, s, uuid(3))
	// The third Session already failed at the version the new image runs.
	if _, err := s.write.Exec(`
		UPDATE sessions SET parse_status = 'failed', parser_version = NULL, parse_attempted_version = ? WHERE id = ?`,
		cur.Version()+1, failed); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = openDir(t, dir, parser.NewRegistry(bumped{cur}))
	got, err := s.EnqueueStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{protocol.SourceClaudeCode: 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("EnqueueStale = %v, want %v", got, want)
	}
	for _, id := range []int64{a, b} {
		if q, ok := queueOf(t, s, id); !ok || q.priority != 1 {
			t.Errorf("session %d queue = %+v, %v", id, q, ok)
		}
	}
	if _, ok := queueOf(t, s, failed); ok {
		t.Error("Session that failed at the current version was re-queued")
	}
	if n, _ := s.Reparsing(ctx); n != 2 {
		t.Errorf("Reparsing = %d, want 2", n)
	}

	// Live ingest still goes first.
	appendTo(t, s, protocol.SourceClaudeCode, claudeKey(uuid(4)), []byte(line1))
	live := sessionIDOf(t, s, uuid(4))
	if job, ok, _, _ := s.NextJob(ctx); !ok || job.SessionID != live {
		t.Errorf("next job = %+v, want live Session %d", job, live)
	}

	// A second start queues nothing new.
	if got, _ := s.EnqueueStale(ctx); got[protocol.SourceClaudeCode] != 0 {
		t.Errorf("second EnqueueStale = %v", got)
	}

	// reparse --session ignores the failure-skip rule.
	n, err := s.Reparse(ctx, ReparseScope{SessionID: failed})
	if err != nil || n != 1 {
		t.Fatalf("Reparse(session) = %d, %v", n, err)
	}
	if q, ok := queueOf(t, s, failed); !ok || q.priority != 1 {
		t.Errorf("failed Session queue = %+v, %v", q, ok)
	}
	if _, err := s.Reparse(ctx, ReparseScope{SessionID: 9999}); !errors.Is(err, ErrNoSession) {
		t.Errorf("Reparse(unknown) err = %v", err)
	}
}

func TestReparseScopes(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	appendTo(t, s, protocol.SourceClaudeCode, claudeKey(uuid(1)), []byte(line1))
	appendTo(t, s, protocol.SourceClaudeCode, claudeKey(uuid(2)), []byte(line1))
	for parseNext(t, s) {
	}
	if n, err := s.Reparse(ctx, ReparseScope{Source: protocol.SourceCodex}); err != nil || n != 0 {
		t.Errorf("Reparse(codex) = %d, %v", n, err)
	}
	if n, err := s.Reparse(ctx, ReparseScope{Source: protocol.SourceClaudeCode}); err != nil || n != 2 {
		t.Errorf("Reparse(claude-code) = %d, %v", n, err)
	}
	for parseNext(t, s) {
	}
	if n, err := s.Reparse(ctx, ReparseScope{}); err != nil || n != 2 {
		t.Errorf("Reparse(all) = %d, %v", n, err)
	}
	// A parent stub has no records and is never queued.
	if _, err := sessionFor(ctx, s.write, "m1", protocol.SourceClaudeCode, "stub"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reparse(ctx, ReparseScope{SessionID: sessionIDOf(t, s, "stub")}); !errors.Is(err, ErrNoSession) {
		t.Errorf("Reparse(stub) err = %v", err)
	}
}

func TestUnattachedRecordAttachesOnLaterStart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openDir(t, dir, parser.Registry{})
	appendTo(t, s, protocol.SourceClaudeCode, mainKey, []byte(line1))
	var n int
	s.read.QueryRow(`SELECT count(*) FROM raw_records WHERE session_id IS NULL`).Scan(&n)
	if n != 1 {
		t.Fatalf("unattached = %d", n)
	}
	if got, err := s.MapUnattached(ctx); err != nil || got != 0 {
		t.Fatalf("MapUnattached with no parser = %d, %v", got, err)
	}
	s.Close()

	s = openDir(t, dir, parser.NewRegistry(claudecode.New()))
	if got, err := s.MapUnattached(ctx); err != nil || got != 1 {
		t.Fatalf("MapUnattached = %d, %v", got, err)
	}
	id := sessionIDOf(t, s, sessUUID)
	if got := rolesOf(t, s, id)[mainKey]; got != "main" {
		t.Errorf("role = %q", got)
	}
	if q, ok := queueOf(t, s, id); !ok || q.priority != 1 {
		t.Errorf("queue = %+v, %v", q, ok)
	}
	if !parseNext(t, s) {
		t.Fatal("nothing to parse")
	}
	if _, msgs, ok, _ := s.Transcript(ctx, id); !ok || len(msgs) != 1 {
		t.Errorf("Transcript ok=%v msgs=%d", ok, len(msgs))
	}
}

func TestHealthCountsWarningsAndFailures(t *testing.T) {
	s := seedFeed(t, []feedSession{
		{machine: "m1", source: "claude-code"},
		{machine: "m1", source: "claude-code"},
		{machine: "m1", source: "claude-code", unparsed: true},
		{machine: "m2-0123456789", source: "claude-code"},
	})
	for _, q := range []string{
		`INSERT INTO parse_warnings (session_id, kind, source_type, count) VALUES (1, 'unknown_type', 'x', 3), (1, 'orphan', '', 1), (4, 'bad_line', '', 1)`,
		`UPDATE sessions SET parse_status = 'failed' WHERE id IN (2, 3)`,
	} {
		if _, err := s.write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	w, f, err := s.Health(context.Background(), "m1")
	if err != nil || w != 1 || f != 2 {
		t.Errorf("Health(m1) = %d, %d, %v; want 1, 2", w, f, err)
	}
	w, f, err = s.Health(context.Background(), "nobody")
	if err != nil || w != 0 || f != 0 {
		t.Errorf("Health(nobody) = %d, %d, %v", w, f, err)
	}
}
