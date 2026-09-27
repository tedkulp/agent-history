package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/tedkulp/agent-history/protocol"
)

// feedSession is one Session row seeded straight into the database.
type feedSession struct {
	machine, source, project string // project "" = No project
	last                     int64
	child                    bool
	unparsed                 bool
}

// seedFeed seeds Sessions on m1 ("laptop") and m2-0123456789 (no name).
func seedFeed(t *testing.T, ss []feedSession) *Store {
	t.Helper()
	s, _ := openParsing(t)
	ctx := context.Background()
	if err := s.UpsertMachine(ctx, "m2-0123456789", protocol.MachineInfo{HomeDir: "/home/x"}); err != nil {
		t.Fatal(err)
	}
	for i, f := range ss {
		var parent any
		if f.child {
			parent = 1
		}
		var parsed any = int64(1)
		if f.unparsed {
			parsed = nil
		}
		var project any
		if f.project != "" {
			project = f.project
		}
		if _, err := s.write.Exec(`
			INSERT INTO sessions (id, machine_id, source, native_id, title, last_activity_at, project_cwd, parent_session_id, parsed_at, parse_status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok')`,
			i+1, f.machine, f.source, fmt.Sprint("n", i+1), fmt.Sprint("t", i+1), f.last, project, parent, parsed); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func ids(rows []FeedRow) []int64 {
	var out []int64
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func strp(s string) *string { return &s }

func TestFeedFilters(t *testing.T) {
	s := seedFeed(t, []feedSession{
		{machine: "m1", source: "claude-code", project: "/Users/ted/src/app", last: 100},     // 1
		{machine: "m1", source: "codex", project: "/Users/ted/src/app", last: 200},           // 2
		{machine: "m1", source: "claude-code", last: 300},                                    // 3: No project
		{machine: "m2-0123456789", source: "claude-code", project: "/home/x/app", last: 400}, // 4
		{machine: "m1", source: "claude-code", last: 500, child: true},                       // 5: child
		{machine: "m1", source: "opencode", last: 600, unparsed: true},                       // 6: never parsed
	})
	ctx := context.Background()
	cases := []struct {
		name string
		f    FeedFilter
		want []int64
	}{
		{"all", FeedFilter{}, []int64{4, 3, 2, 1}},
		{"machine", FeedFilter{Machine: "m1"}, []int64{3, 2, 1}},
		{"source", FeedFilter{Source: "claude-code"}, []int64{4, 3, 1}},
		{"project", FeedFilter{Machine: "m1", Project: strp("/Users/ted/src/app")}, []int64{2, 1}},
		{"no project", FeedFilter{Machine: "m1", Project: strp("")}, []int64{3}},
		{"combined", FeedFilter{Machine: "m1", Project: strp("/Users/ted/src/app"), Source: "codex"}, []int64{2}},
		{"unknown machine", FeedFilter{Machine: "nope"}, nil},
	}
	for _, c := range cases {
		rows, err := s.Feed(ctx, c.f, 50)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(rows); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ids = %v, want %v", c.name, got, c.want)
		}
	}

	rows, _ := s.Feed(ctx, FeedFilter{}, 50)
	if rows[0].Machine != "m2-01234" || rows[0].MachineID != "m2-0123456789" || rows[1].Machine != "laptop" {
		t.Errorf("machine labels = %q, %q", rows[0].Machine, rows[1].Machine)
	}
}

func TestFeedPagingHasNoGapsOrDuplicates(t *testing.T) {
	// Ties on last_activity_at straddle every page boundary.
	var ss []feedSession
	for i := 0; i < 11; i++ {
		ss = append(ss, feedSession{machine: "m1", source: "claude-code", last: int64(1000 - i/3)})
	}
	s := seedFeed(t, ss)
	ctx := context.Background()
	var got []int64
	f := FeedFilter{}
	for page := 0; page < 10; page++ {
		rows, err := s.Feed(ctx, f, 4)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ids(rows)...)
		if len(rows) < 4 {
			break
		}
		last := rows[len(rows)-1]
		f.Before = &Cursor{At: last.LastActivityAt, ID: last.ID}
	}
	want := []int64{3, 2, 1, 6, 5, 4, 9, 8, 7, 11, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}
}

func TestFeedChips(t *testing.T) {
	s := seedFeed(t, []feedSession{
		{machine: "m1", source: "claude-code", project: "/Users/ted/src/app", last: 100},
		{machine: "m1", source: "codex", project: "/Users/ted/src/app", last: 200},
		{machine: "m1", source: "claude-code", last: 300},
		{machine: "m1", source: "claude-code", project: "/Users/ted/src/lib", last: 50},
		{machine: "m1", source: "opencode", project: "/Users/ted/src/lib", last: 999, child: true},
		{machine: "m2-0123456789", source: "claude-code", project: "/home/x/app", last: 400},
	})
	ctx := context.Background()

	machines, err := s.FeedMachines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []MachineChip{{ID: "m1", Label: "laptop"}, {ID: "m2-0123456789", Label: "m2-01234"}}
	if !reflect.DeepEqual(machines, want) {
		t.Errorf("machines = %+v", machines)
	}

	sources, err := s.FeedSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sources, []string{"claude-code", "codex"}) {
		t.Errorf("sources = %v", sources)
	}

	projects, err := s.MachineProjects(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	wantP := []ProjectCount{{Cwd: "/Users/ted/src/app", Sessions: 2}, {Cwd: "/Users/ted/src/lib", Sessions: 1}, {Cwd: "", Sessions: 1}}
	if !reflect.DeepEqual(projects, wantP) {
		t.Errorf("projects = %+v", projects)
	}
}

func TestFeedWarningsAndDriftBanners(t *testing.T) {
	now := int64(1_800_000_000_000) // openParsing's clock
	day := int64(24 * 3600 * 1000)
	s := seedFeed(t, []feedSession{
		{machine: "m1", source: "claude-code", last: now - day},        // 1: warnings, 2.1.1, recent
		{machine: "m1", source: "claude-code", last: now - 2*day},      // 2: warnings, 2.1.1, recent
		{machine: "m1", source: "claude-code", last: now - 30*day},     // 3: warnings, 2.1.0, old
		{machine: "m1", source: "claude-code", last: now},              // 4: failed, no warnings
		{machine: "m1", source: "codex", last: now},                    // 5: clean
		{machine: "m1", source: "codex", last: now - day, child: true}, // 6: child with warnings, 0.52
	})
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE sessions SET source_version = '2.1.1' WHERE id IN (1, 2)`,
		`UPDATE sessions SET source_version = '2.1.0' WHERE id = 3`,
		`UPDATE sessions SET source_version = '0.52' WHERE id = 6`,
		`UPDATE sessions SET parse_status = 'failed' WHERE id = 4`,
		`INSERT INTO parse_warnings (session_id, kind, source_type, count) VALUES (1, 'unknown_type', 'x', 3), (1, 'orphan', '', 1), (2, 'bad_line', '', 1), (3, 'unknown_type', 'y', 1), (6, 'unknown_type', 'z', 1)`,
	} {
		if _, err := s.write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.Feed(ctx, FeedFilter{Warnings: true}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(rows), []int64{4, 1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("warnings feed = %v, want %v", got, want)
	}
	rows, _ = s.Feed(ctx, FeedFilter{Warnings: true, Source: "codex"}, 50)
	if len(rows) != 0 {
		t.Errorf("warnings+codex feed = %v", ids(rows))
	}

	drift, err := s.DriftBanners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Drift{{Source: "claude-code", SourceVersion: "2.1.1", Sessions: 2}, {Source: "codex", SourceVersion: "0.52", Sessions: 1}}
	if !reflect.DeepEqual(drift, want) {
		t.Errorf("drift = %+v", drift)
	}
}
