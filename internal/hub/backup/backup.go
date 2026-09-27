// Package backup names, schedules and prunes the Hub's backup files
// (hub.md §4.8). The copy itself is store.Backup.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/config"
)

// scheduledGlob matches scheduled backups only: manual backups
// (hub-YYYYMMDD-HHMMSS.db) and pre-migrate-*.db never match it.
const scheduledGlob = "hub-????????.db"

// ScheduledName is the file name of the scheduled backup taken at t.
func ScheduledName(t time.Time) string { return t.Format("hub-20060102.db") }

// ManualName is the file name of an `agent-history-hub backup` taken at t.
func ManualName(t time.Time) string { return t.Format("hub-20060102-150405.db") }

// Next is the first time after now at the wall-clock time at, in now's
// location.
func Next(now time.Time, at config.TimeOfDay) time.Time {
	y, m, d := now.Date()
	next := time.Date(y, m, d, at.Hour, at.Minute, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(y, m, d+1, at.Hour, at.Minute, 0, 0, now.Location())
	}
	return next
}

// Prune deletes all but the newest keep scheduled backups in dir and returns
// the paths it deleted.
func Prune(dir string, keep int) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, scheduledGlob))
	if err != nil {
		return nil, err
	}
	slices.Sort(files) // YYYYMMDD sorts oldest first
	var removed []string
	var errs []error
	for _, f := range files[:max(len(files)-keep, 0)] {
		if err := os.Remove(f); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, f)
	}
	return removed, errors.Join(errs...)
}

// Store is the part of the store a backup needs.
type Store interface {
	Backup(ctx context.Context, dst string) error
}

// Scheduler takes the daily backup.
type Scheduler struct {
	Store Store
	Dir   string
	At    config.TimeOfDay
	Keep  int
	Log   *slog.Logger
	Now   func() time.Time // time.Now when nil
}

func (s *Scheduler) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Run takes a backup at every scheduled time until ctx is done. A failure is
// logged and the next day's backup runs as usual.
func (s *Scheduler) Run(ctx context.Context) {
	var prev time.Time
	for {
		// From prev too, so a timer that fires early can't repeat a day.
		now := s.now()
		if prev.After(now) {
			now = prev
		}
		prev = Next(now, s.At)
		t := time.NewTimer(prev.Sub(s.now()))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.Log.Error("scheduled backup failed", "err", err)
		}
	}
}

// RunOnce takes today's scheduled backup, then prunes old ones.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	start := time.Now()
	dst := filepath.Join(s.Dir, ScheduledName(s.now()))
	if err := s.Store.Backup(ctx, dst); err != nil {
		return err
	}
	s.Log.Info("wrote backup", "path", dst, "duration", time.Since(start))
	removed, err := Prune(s.Dir, s.Keep)
	for _, f := range removed {
		s.Log.Info("pruned backup", "path", f)
	}
	if err != nil {
		return fmt.Errorf("pruning backups: %w", err)
	}
	return nil
}
