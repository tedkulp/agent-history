package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writer opens path the way launchd does and returns it with its fd.
func writer(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestRotatesPastLimitKeepingThreeFiles(t *testing.T) {
	log := filepath.Join(t.TempDir(), "collector.log")
	w := writer(t, log)
	r := &Rotator{Path: log, MaxSize: 10, Fds: []int{int(w.Fd())}}

	for i, gen := range []string{"first-generation\n", "second-generation\n", "third-generation\n", "fourth-generation\n"} {
		w.WriteString(gen)
		rotated, err := r.Rotate()
		if err != nil || !rotated {
			t.Fatalf("rotation %d: rotated %v, err %v", i, rotated, err)
		}
	}
	w.WriteString("live\n")

	if got := read(t, log); got != "live\n" {
		t.Fatalf("collector.log = %q: writes after a rotation must reach the fresh file", got)
	}
	if got := read(t, log+".1"); got != "fourth-generation\n" {
		t.Fatalf(".1 = %q", got)
	}
	if got := read(t, log+".2"); got != "third-generation\n" {
		t.Fatalf(".2 = %q", got)
	}
	if _, err := os.Stat(log + ".3"); !os.IsNotExist(err) {
		t.Fatalf(".3 exists (err %v), want 3 files in all", err)
	}
}

func TestLeavesFileAtOrUnderLimit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "collector.log")
	w := writer(t, log)
	w.WriteString(strings.Repeat("x", 10))
	r := &Rotator{Path: log, MaxSize: 10, Fds: []int{int(w.Fd())}}
	if rotated, err := r.Rotate(); rotated || err != nil {
		t.Fatalf("rotated %v, err %v; want no rotation at the limit", rotated, err)
	}
	if _, err := os.Stat(log + ".1"); !os.IsNotExist(err) {
		t.Fatal(".1 created without a rotation")
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	r := &Rotator{Path: filepath.Join(t.TempDir(), "collector.log")}
	if rotated, err := r.Rotate(); rotated || err != nil {
		t.Fatalf("rotated %v, err %v", rotated, err)
	}
}

func TestDefaultsAreTenMegabytesAndThreeFiles(t *testing.T) {
	log := filepath.Join(t.TempDir(), "collector.log")
	w := writer(t, log)
	r := &Rotator{Path: log, Fds: []int{int(w.Fd())}}
	w.Write(make([]byte, 10<<20))
	if rotated, _ := r.Rotate(); rotated {
		t.Fatal("rotated at exactly 10 MB")
	}
	w.WriteString("x")
	if rotated, err := r.Rotate(); !rotated || err != nil {
		t.Fatalf("rotated %v, err %v; want a rotation just over 10 MB", rotated, err)
	}
}

func TestStderrOnlyWhenStderrIsTheLog(t *testing.T) {
	if r := Stderr(filepath.Join(t.TempDir(), "collector.log")); r != nil {
		t.Fatal("got a Rotator for a file stderr doesn't point at")
	}
}

func TestStderrWhenStderrIsTheLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "collector.log")
	orig := os.Stderr
	t.Cleanup(func() { os.Stderr = orig })
	os.Stderr = writer(t, log)
	if r := Stderr(log); r == nil || r.Path != log {
		t.Fatalf("got %+v, want a Rotator for %s", r, log)
	}
}
