package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/protocol"
)

// twoLayouts is a fake Source with two Layouts, like opencode's legacy JSON
// files (rank 1) and its database (rank 2):
//
//	legacy/<id>.json      main,       Layout "legacy", rank 1
//	legacy/<id>/<name>    attachment, Layout "legacy", rank 1
//	db/<id>/<n>           main,       Layout "db",     rank 2
type twoLayouts struct{}

const fakeSource = "fake"

func (twoLayouts) Source() string { return fakeSource }
func (twoLayouts) Version() int   { return 1 }
func (twoLayouts) Parse(parser.Input) (parser.Result, error) {
	return parser.Result{}, nil
}

func (twoLayouts) MapKey(key string) (parser.Mapping, bool) {
	layout, rest, ok := strings.Cut(key, "/")
	if !ok {
		return parser.Mapping{}, false
	}
	switch layout {
	case "legacy":
		if id, ok := strings.CutSuffix(rest, ".json"); ok && !strings.Contains(id, "/") {
			return parser.Mapping{NativeID: id, Role: parser.RoleMain, Layout: "legacy", LayoutRank: 1}, true
		}
		if id, _, ok := strings.Cut(rest, "/"); ok {
			return parser.Mapping{NativeID: id, Role: parser.RoleAttachment, Layout: "legacy", LayoutRank: 1}, true
		}
	case "db":
		if id, _, ok := strings.Cut(rest, "/"); ok {
			return parser.Mapping{NativeID: id, Role: parser.RoleMain, Layout: "db", LayoutRank: 2}, true
		}
	}
	return parser.Mapping{}, false
}

func openTwoLayouts(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir(), parser.NewRegistry(twoLayouts{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// rolesOf maps each Record key of a Session to its role.
func rolesOf(t *testing.T, s *Store, sessionID int64) map[string]string {
	t.Helper()
	rows, err := s.read.Query(`SELECT record_key, role FROM raw_records WHERE session_id = ?`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, r string
		if err := rows.Scan(&k, &r); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

// drain empties the parse queue.
func drain(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.write.Exec(`DELETE FROM parse_queue`); err != nil {
		t.Fatal(err)
	}
}

func TestHigherRankedLayoutWinsAndShadowsTheOther(t *testing.T) {
	s := openTwoLayouts(t)
	ctx := context.Background()
	appendTo(t, s, fakeSource, "legacy/s1.json", []byte("{}"))
	appendTo(t, s, fakeSource, "legacy/s1/part1", []byte("{}"))
	id := sessionIDOf(t, s, "s1")
	want := map[string]string{"legacy/s1.json": "main", "legacy/s1/part1": "attachment"}
	if got := rolesOf(t, s, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}

	// The database Layout appears: it wins, and the Session re-parses.
	drain(t, s)
	appendTo(t, s, fakeSource, "db/s1/1", []byte("row"))
	want = map[string]string{"legacy/s1.json": "shadow", "legacy/s1/part1": "shadow", "db/s1/1": "main"}
	if got := rolesOf(t, s, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if q, ok := queueOf(t, s, id); !ok || q.priority != 0 {
		t.Fatalf("new winner not live-enqueued: %+v %v", q, ok)
	}
	in, ok, err := s.LoadParseInput(ctx, id)
	if err != nil || !ok || in.Input.MainKey != "db/s1/1" || len(in.Input.Attachments) != 0 {
		t.Fatalf("parse input = %+v, %v, %v", in.Input, ok, err)
	}

	// Records of the losing Layout are shadow and never enqueue, new or
	// appended to.
	drain(t, s)
	appendTo(t, s, fakeSource, "legacy/s1/part2", []byte("{}"))
	appendTo(t, s, fakeSource, "legacy/s1.json", []byte("more"))
	if got := rolesOf(t, s, id)["legacy/s1/part2"]; got != "shadow" {
		t.Errorf("late legacy attachment role = %q", got)
	}
	if _, ok := queueOf(t, s, id); ok {
		t.Error("a shadow record enqueued the Session")
	}
}

func TestLowerRankedLayoutArrivingLaterIsShadow(t *testing.T) {
	s := openTwoLayouts(t)
	appendTo(t, s, fakeSource, "db/s1/1", []byte("row"))
	appendTo(t, s, fakeSource, "legacy/s1.json", []byte("{}"))
	want := map[string]string{"db/s1/1": "main", "legacy/s1.json": "shadow"}
	if got := rolesOf(t, s, sessionIDOf(t, s, "s1")); !reflect.DeepEqual(got, want) {
		t.Errorf("roles = %v, want %v", got, want)
	}
}

func TestExtraMainInWinningLayoutIsShadow(t *testing.T) {
	s := openTwoLayouts(t)
	ctx := context.Background()
	appendTo(t, s, fakeSource, "db/s1/1", []byte("old"))
	appendTo(t, s, fakeSource, "db/s1/2", []byte("new"))
	id := sessionIDOf(t, s, "s1")
	want := map[string]string{"db/s1/1": "shadow", "db/s1/2": "main"}
	if got := rolesOf(t, s, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if in, _, _ := s.LoadParseInput(ctx, id); in.Input.MainKey != "db/s1/2" {
		t.Errorf("parsed main = %q", in.Input.MainKey)
	}
}

func TestAttachmentBeforeMainWaits(t *testing.T) {
	s := openTwoLayouts(t)
	ctx := context.Background()
	appendTo(t, s, fakeSource, "legacy/s1/part1", []byte("{}"))
	id := sessionIDOf(t, s, "s1")
	if got := rolesOf(t, s, id)["legacy/s1/part1"]; got != "attachment" {
		t.Fatalf("role = %q", got)
	}
	if _, ok, err := s.LoadParseInput(ctx, id); ok || err != nil {
		t.Fatalf("parse input without a main: ok=%v err=%v", ok, err)
	}
	appendTo(t, s, fakeSource, "legacy/s1.json", []byte("{}"))
	in, ok, err := s.LoadParseInput(ctx, id)
	if err != nil || !ok || in.Input.MainKey != "legacy/s1.json" || len(in.Input.Attachments) != 1 {
		t.Fatalf("parse input = %+v, %v, %v", in.Input, ok, err)
	}
}

func TestUnmappedRecordIsStoredUnattached(t *testing.T) {
	s := openTwoLayouts(t)
	appendTo(t, s, fakeSource, "unknown-layout/x", []byte("x"))
	appendTo(t, s, protocol.SourceCodex, "x.jsonl", []byte("x"))
	var n int
	if err := s.read.QueryRow(`SELECT count(*) FROM raw_records WHERE session_id IS NULL AND role IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Errorf("unattached records = %d, %v", n, err)
	}
}
