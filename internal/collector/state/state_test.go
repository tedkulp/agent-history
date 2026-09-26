package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestDefaultDir(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := DefaultDir(getenv, "/home/ted"); got != "/home/ted/.local/state/agent-history" {
		t.Fatalf("default = %s", got)
	}
	env["XDG_STATE_HOME"] = "/xdg/state"
	if got := DefaultDir(getenv, "/home/ted"); got != "/xdg/state/agent-history" {
		t.Fatalf("with XDG_STATE_HOME = %s", got)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: err = %v, want ErrLocked", err)
	}
	unlock()
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}
