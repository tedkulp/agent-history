// Package logfile rotates the Collector's macOS log (collector.md §4.10).
// launchd points stdout and stderr at collector.log and never rotates it, so
// the Collector does: once the file is over the size limit, it shifts it to
// .1 and .2 and points its own stdout and stderr at a fresh file.
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Spec values: rotate past 10 MB, keeping 3 files in all.
const (
	MaxSize = 10 << 20
	Keep    = 3
)

// Rotator rotates one log file that this process writes to through fds.
type Rotator struct {
	Path    string
	MaxSize int64 // MaxSize when zero
	Keep    int   // files kept, the live one included; Keep when zero
	// Fds are redirected to the fresh file after a rotation: stdout and
	// stderr when empty.
	Fds []int
}

// Stderr returns a Rotator for path if this process's stderr is that file,
// as it is under launchd, and nil otherwise (a terminal, or journald).
func Stderr(path string) *Rotator {
	errFi, err := os.Stderr.Stat()
	if err != nil {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil || !os.SameFile(errFi, fi) {
		return nil
	}
	return &Rotator{Path: path}
}

// Rotate rotates the file if it is over the size limit, and reports whether
// it did.
func (r *Rotator) Rotate() (bool, error) {
	max, keep, fds := r.MaxSize, r.Keep, r.Fds
	if max == 0 {
		max = MaxSize
	}
	if keep == 0 {
		keep = Keep
	}
	if len(fds) == 0 {
		fds = []int{1, 2}
	}
	fi, err := os.Stat(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Size() <= max {
		return false, nil
	}
	name := func(i int) string {
		if i == 0 {
			return r.Path
		}
		return fmt.Sprintf("%s.%d", r.Path, i)
	}
	if err := os.Remove(name(keep - 1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for i := keep - 2; i >= 0; i-- {
		if err := os.Rename(name(i), name(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	f, err := os.OpenFile(r.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return true, err
	}
	defer f.Close()
	for _, fd := range fds {
		if err := unix.Dup2(int(f.Fd()), fd); err != nil {
			return true, fmt.Errorf("redirecting fd %d to %s: %w", fd, r.Path, err)
		}
	}
	return true, nil
}
