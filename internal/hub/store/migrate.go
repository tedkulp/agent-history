package store

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations returns the embedded migrations in order. File names are
// "NNNN_name.sql" and must be numbered 1, 2, 3… without gaps.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: name must be NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: expected number %d", m.name, i+1)
		}
	}
	return out, nil
}

// migrate applies pending migrations on the writer, each in its own
// transaction that also sets PRAGMA user_version (hub.md §4.1 steps 3 to 5).
// A database from a newer Hub is refused. A database that isn't new is first
// copied to backupDir/pre-migrate-<user_version>.db; if that fails, nothing
// is migrated.
func (s *Store) migrate(ctx context.Context, backupDir string) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	var current int
	if err := s.write.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return err
	}
	if current > len(migs) {
		return fmt.Errorf("database is from a newer Hub (schema %d, this Hub knows %d); restore a pre-migrate backup to roll back", current, len(migs))
	}
	if current > 0 && current < len(migs) {
		if backupDir == "" {
			return fmt.Errorf("schema %d needs migrating and no backup dir is set for the pre-migrate backup", current)
		}
		dst := filepath.Join(backupDir, fmt.Sprintf("pre-migrate-%d.db", current))
		if err := vacuumInto(ctx, s.write, dst); err != nil {
			return fmt.Errorf("pre-migrate backup, not migrating: %w", err)
		}
		s.log.Info("wrote pre-migrate backup", "path", dst)
	}
	for _, m := range migs[current:] {
		if err := s.apply(ctx, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		s.log.Info("applied migration", "name", m.name)
	}
	return nil
}

func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return err
	}
	return tx.Commit()
}
