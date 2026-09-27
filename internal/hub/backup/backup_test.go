package backup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/config"
)

func TestNames(t *testing.T) {
	at := time.Date(2026, 9, 7, 3, 4, 5, 0, time.UTC)
	if got := ScheduledName(at); got != "hub-20260907.db" {
		t.Errorf("ScheduledName = %q", got)
	}
	if got := ManualName(at); got != "hub-20260907-030405.db" {
		t.Errorf("ManualName = %q", got)
	}
}

func TestNext(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	at := config.TimeOfDay{Hour: 3}
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 9, 7, 2, 59, 0, 0, ny), time.Date(2026, 9, 7, 3, 0, 0, 0, ny)},
		{time.Date(2026, 9, 7, 3, 0, 0, 0, ny), time.Date(2026, 9, 8, 3, 0, 0, 0, ny)},
		{time.Date(2026, 9, 7, 23, 0, 0, 0, ny), time.Date(2026, 9, 8, 3, 0, 0, 0, ny)},
		// Across the end of DST the wall-clock time stays 03:00.
		{time.Date(2026, 10, 31, 12, 0, 0, 0, ny), time.Date(2026, 11, 1, 3, 0, 0, 0, ny)},
	}
	for _, c := range cases {
		if got := Next(c.now, at); !got.Equal(c.want) {
			t.Errorf("Next(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestPruneKeepsNewestScheduledOnly(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir,
		"hub-20260901.db", "hub-20260902.db", "hub-20260903.db", "hub-20260904.db",
		"hub-20260901-120000.db", "pre-migrate-1.db", "hub-20260905.db.tmp", "other.db")
	removed, err := Prune(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %v", removed)
	}
	want := []string{"hub-20260901-120000.db", "hub-20260903.db", "hub-20260904.db", "hub-20260905.db.tmp", "other.db", "pre-migrate-1.db"}
	if got := ls(t, dir); !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
}

// fakeStore writes an empty file per backup, or fails while err is set.
type fakeStore struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakeStore) Backup(_ context.Context, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, filepath.Base(dst))
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(dst, nil, 0o644)
}

func TestRunOnceBacksUpThenPrunes(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "hub-20260901.db", "hub-20260902.db", "hub-20260903.db")
	now := time.Date(2026, 9, 4, 3, 0, 0, 0, time.Local)
	s := &Scheduler{Store: &fakeStore{}, Dir: dir, Keep: 2, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := ls(t, dir), []string{"hub-20260903.db", "hub-20260904.db"}; !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
}

func TestRunRetriesAfterFailureAtNextScheduledTime(t *testing.T) {
	dir := t.TempDir()
	st := &fakeStore{err: errors.New("disk on fire")}
	// A clock that runs fast: each call is a day on, so every timer is due.
	var mu sync.Mutex
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	s := &Scheduler{Store: st, Dir: dir, At: config.TimeOfDay{Hour: 3}, Keep: 7, Log: slog.New(slog.DiscardHandler), Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		day = day.AddDate(0, 0, 1)
		return day
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			st.mu.Lock()
			got := len(st.calls)
			st.mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("fewer than %d backups", n)
	}
	waitFor(1)
	st.mu.Lock()
	st.err = nil
	st.mu.Unlock()
	waitFor(3)
	cancel()
	<-done
	if files, _ := filepath.Glob(filepath.Join(dir, scheduledGlob)); len(files) == 0 {
		t.Error("no backup written after the failure")
	}
}
