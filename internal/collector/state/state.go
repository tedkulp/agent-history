// Package state locates the Collector's state directory and holds its lock
// (collector.md §2.4).
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another process holds collector.lock.
var ErrLocked = errors.New("already running")

// DefaultDir is $XDG_STATE_HOME/agent-history, defaulting to
// ~/.local/state/agent-history on every OS.
func DefaultDir(getenv func(string) string, home string) string {
	base := getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "agent-history")
}

// Lock creates dir if needed and takes the flock on dir/collector.lock, held
// by the one process that owns uploads. It returns ErrLocked when another
// process holds it. The returned func releases the lock.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "collector.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("locking %s: %w", f.Name(), err)
	}
	// Closing the file releases the flock.
	return func() { f.Close() }, nil
}
