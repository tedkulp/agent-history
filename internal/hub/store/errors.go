package store

import (
	"errors"
	"fmt"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Ingest errors the API maps to status codes (hub.md §4.10).
var (
	// ErrBusy means the database stayed locked beyond busy_timeout.
	ErrBusy = errors.New("database is locked")
	// ErrDiskFull means SQLite could not grow the database or its WAL.
	ErrDiskFull = errors.New("disk is full")
)

// classify wraps a SQLite busy or disk-full error in ErrBusy or ErrDiskFull,
// keeping the original in the chain. Other errors pass through unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	switch se.Code() & 0xff { // primary code of an extended result code
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case sqlite3.SQLITE_FULL:
		return fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	return err
}
