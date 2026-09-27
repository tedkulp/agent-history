package runner

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/status"
)

// An unclaimed file is reported by status and logged once, even across
// restarts; memory notes are known-ignored; neither is shipped.
func TestUnclaimedAndKnownIgnoredPaths(t *testing.T) {
	f := newFixture(t)
	stray := proj + "/" + sess + "/notes.bin"
	memory := proj + "/memory/MEMORY.md"
	f.write(key, cwdLine("/Users/ted/src/app"))
	f.write(stray, "?")
	f.write(memory, "# notes\n")

	var mu sync.Mutex
	var logs bytes.Buffer
	run := func() status.Report {
		t.Helper()
		cfg := f.config()
		cfg.Cache = cache.Load(f.cachePath(), "hub", nil)
		cfg.Log = slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))
		cfg.RescanInterval = 20 * time.Millisecond
		cfg.Control = NewControl()
		stop := f.start(cfg)
		var rep status.Report
		waitFor(t, 5*time.Second, "startup reconcile and a few rescans", func() bool {
			rep = f.status(cfg.Control)
			return rep.Hub.LastSync != nil && time.Since(*rep.Hub.LastSync) > 100*time.Millisecond
		})
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		return rep
	}

	for i := range 2 {
		rep := run()
		s := rep.Sources[0]
		if s.Unclaimed != 1 || !slices.Equal(s.UnclaimedPaths, []string{stray}) {
			t.Errorf("run %d: unclaimed %d %q", i, s.Unclaimed, s.UnclaimedPaths)
		}
		if s.Ignored != 1 || len(s.IgnoredPaths) != 1 || s.IgnoredPaths[0].Path != memory || s.IgnoredPaths[0].Modified.IsZero() {
			t.Errorf("run %d: ignored %d %+v", i, s.Ignored, s.IgnoredPaths)
		}
	}

	if !f.onHub(key) {
		t.Error("the Session was not shipped")
	}
	if f.onHub(stray) || f.onHub(memory) {
		t.Error("an unclaimed or known-ignored file was shipped")
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "unclaimed file"); n != 1 {
		t.Errorf("unclaimed file logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), stray) {
		t.Errorf("no warn naming %s:\n%s", stray, logs.String())
	}
}
