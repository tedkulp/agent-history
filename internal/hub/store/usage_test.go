package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
)

// saveResult saves res as the parse of a new Session on m1.
func saveResult(t *testing.T, s *Store, source, nativeID string, res parser.Result) {
	t.Helper()
	ctx := context.Background()
	id, err := sessionFor(ctx, s.write, "m1", source, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveParse(ctx, Job{SessionID: id}, 1, res); err != nil {
		t.Fatal(err)
	}
}

func usageMsg(id string, at int64, model string, u *parser.Usage) parser.Message {
	role := parser.MessageUser
	if model != "" {
		role = parser.MessageAssistant
	}
	return parser.Message{ID: id, Role: role, Timestamp: at, Model: model, Usage: u,
		Parts: []parser.Part{{ID: id + ".0", Kind: parser.KindText, Payload: parser.TextPayload{Text: id}}}}
}

func TestUsageReport(t *testing.T) {
	s, _ := openParsing(t)
	ctx := context.Background()
	loc := time.FixedZone("X", -4*3600)
	at := func(day, hour, min int) int64 { return time.Date(2026, 10, day, hour, min, 0, 0, loc).UnixMilli() }
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, loc) }

	// A spans midnight; its Child Session works in another directory.
	saveResult(t, s, "claude-code", "A", parser.Result{
		Session: parser.Session{StartedAt: at(10, 23, 0), Cwd: "/Users/ted/src/app"},
		Messages: []parser.Message{
			usageMsg("u1", at(10, 23, 0), "", nil),
			usageMsg("a1", at(10, 23, 30), "opus", &parser.Usage{Input: 10, Output: 1}),
			usageMsg("a2", at(11, 0, 30), "opus", &parser.Usage{Input: 20, Output: 2}),
		},
	})
	saveResult(t, s, "claude-code", "C", parser.Result{
		Session:  parser.Session{StartedAt: at(11, 1, 0), Cwd: "/Users/ted/src/other", ParentNativeID: "A"},
		Messages: []parser.Message{usageMsg("c1", at(11, 1, 0), "haiku", &parser.Usage{Input: 5})},
	})
	// H has Session-level Usage only, dated by its start the day before.
	saveResult(t, s, "hermes", "H", parser.Result{
		Session: parser.Session{StartedAt: at(9, 12, 0), Cwd: "/Users/ted/src/h", Usage: &parser.Usage{Input: 100}},
		Messages: []parser.Message{
			usageMsg("h1", at(10, 10, 0), "", nil),
			usageMsg("h2", 0, "gpt", nil), // no timestamp: dated by its Session's start
		},
	})
	// N reported no Usage; O is long before the window.
	saveResult(t, s, "claude-code", "N", parser.Result{Messages: []parser.Message{usageMsg("n1", at(10, 9, 0), "", nil)}})
	saveResult(t, s, "claude-code", "O", parser.Result{
		Session:  parser.Session{StartedAt: at(1, 9, 0), Cwd: "/Users/ted/src/app"},
		Messages: []parser.Message{usageMsg("o1", at(1, 9, 0), "opus", &parser.Usage{Input: 1000})},
	})

	since := day(9).UnixMilli()
	rep, err := s.UsageReport(ctx, since, loc, UsageBySource)
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := UsageCounts{Usage: parser.Usage{Input: 135, Output: 3}, Sessions: 3, Messages: 7}
	if rep.Total != wantTotal {
		t.Errorf("total = %+v, want %+v", rep.Total, wantTotal)
	}
	if rep.NoUsage != 1 {
		t.Errorf("no Usage = %d, want 1", rep.NoUsage)
	}
	wantDays := []UsageDay{
		{Day: day(9), Usage: parser.Usage{Input: 100}},
		{Day: day(10), Usage: parser.Usage{Input: 10, Output: 1}},
		{Day: day(11), Usage: parser.Usage{Input: 25, Output: 2}},
	}
	if !reflect.DeepEqual(rep.Days, wantDays) {
		t.Errorf("days =\n%+v\nwant\n%+v", rep.Days, wantDays)
	}
	wantSources := []UsageGroup{
		{Key: "hermes", Label: "hermes", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 100}, Sessions: 1, Messages: 2}},
		{Key: "claude-code", Label: "claude-code", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 35, Output: 3}, Sessions: 2, Messages: 5}},
	}
	if !reflect.DeepEqual(rep.Groups, wantSources) {
		t.Errorf("by source =\n%+v\nwant\n%+v", rep.Groups, wantSources)
	}

	// The Child Session's tokens count under its parent's Project.
	rep, err = s.UsageReport(ctx, since, loc, UsageByProject)
	if err != nil {
		t.Fatal(err)
	}
	wantProjects := []UsageGroup{
		{Key: "/Users/ted/src/h", Label: "/Users/ted/src/h", Machine: "laptop", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 100}, Sessions: 1, Messages: 2}},
		{Key: "/Users/ted/src/app", Label: "/Users/ted/src/app", Machine: "laptop", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 35, Output: 3}, Sessions: 1, Messages: 4}},
		{Key: "", Label: "", Machine: "laptop", UsageCounts: UsageCounts{Sessions: 1, Messages: 1}},
	}
	if !reflect.DeepEqual(rep.Groups, wantProjects) {
		t.Errorf("by project =\n%+v\nwant\n%+v", rep.Groups, wantProjects)
	}

	// A Message's model is its own, else its Session's, else "unknown".
	rep, err = s.UsageReport(ctx, since, loc, UsageByModel)
	if err != nil {
		t.Fatal(err)
	}
	wantModels := []UsageGroup{
		{Key: "gpt", Label: "gpt", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 100}, Sessions: 1, Messages: 2}},
		{Key: "opus", Label: "opus", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 30, Output: 3}, Sessions: 1, Messages: 3}},
		{Key: "haiku", Label: "haiku", UsageCounts: UsageCounts{Usage: parser.Usage{Input: 5}, Messages: 1}},
		{Key: "unknown", Label: "unknown", UsageCounts: UsageCounts{Sessions: 1, Messages: 1}},
	}
	if !reflect.DeepEqual(rep.Groups, wantModels) {
		t.Errorf("by model =\n%+v\nwant\n%+v", rep.Groups, wantModels)
	}

	rep, err = s.UsageReport(ctx, since, loc, UsageByMachine)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Groups) != 1 || rep.Groups[0].Key != "m1" || rep.Groups[0].Label != "laptop" || rep.Groups[0].UsageCounts != wantTotal {
		t.Errorf("by machine = %+v", rep.Groups)
	}

	// The whole history reaches O.
	rep, err = s.UsageReport(ctx, 0, loc, UsageBySource)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Input != 1135 || rep.Total.Sessions != 4 || len(rep.Days) != 4 || rep.Days[0].Day != day(1) {
		t.Errorf("all time: total = %+v, days = %+v", rep.Total, rep.Days)
	}
}

func TestUsageReportEmpty(t *testing.T) {
	s, _ := openParsing(t)
	rep, err := s.UsageReport(context.Background(), 0, time.UTC, UsageByModel)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep, UsageReport{}) {
		t.Errorf("report = %+v", rep)
	}
}
