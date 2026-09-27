package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/protocol"
)

// shim is a fake `<shim> version`.
type shim struct {
	version atomic.Value // string
	fail    atomic.Bool
	calls   atomic.Int32
}

func newShim(v string) *shim {
	s := &shim{}
	s.version.Store(v)
	return s
}

func (s *shim) check(ctx context.Context) (string, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return "", errors.New("exec: no such file")
	}
	return s.version.Load().(string), nil
}

// run starts Run and returns its result channel; the test's end cancels it.
func (f *fixture) run(cfg Config) <-chan error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	})
	return done
}

func TestNewVersionRestartsAfterDrainingAndWritingCache(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	sh := newShim("0.3.0")
	cfg := f.config()
	cfg.Info.CollectorVersion = "0.3.0"
	cfg.RescanInterval = 50 * time.Millisecond
	cfg.CheckVersion = sh.check
	done := f.run(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })
	waitFor(t, 5*time.Second, "a version check", func() bool { return sh.calls.Load() > 0 })

	// An upload is in flight when `mise upgrade` lands.
	release := make(chan struct{})
	f.setBlock(release)
	f.appendLine(key, "{\"n\":2}")
	<-f.blocked
	sh.version.Store("0.4.0")
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) before the in-flight upload finished", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil so the process exits 0", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after a new version was found")
	}

	if !f.hubHas(key) {
		t.Fatal("the drained upload didn't reach the Hub")
	}
	e, ok := cache.Load(f.cachePath(), "hub", nil).Get(cache.Key(protocol.SourceClaudeCode, key))
	if !ok || e.Length != int64(len("{\"n\":1}\n{\"n\":2}\n")) {
		t.Fatalf("cache on disk has %+v (ok %v), want the drained upload's state", e, ok)
	}
}

func TestRestartDrainGivesUp(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	sh := newShim("0.3.0")
	cfg := f.config()
	cfg.Info.CollectorVersion = "0.3.0"
	cfg.RescanInterval = 50 * time.Millisecond
	cfg.RestartDrain = 200 * time.Millisecond
	cfg.CheckVersion = sh.check
	done := f.run(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	f.setBlock(never)
	f.appendLine(key, "{\"n\":2}")
	<-f.blocked
	sh.version.Store("0.4.0")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart waited past its drain timeout")
	}
}

func TestSameVersionKeepsRunning(t *testing.T) {
	f := newFixture(t)
	sh := newShim("0.3.0")
	cfg := f.config()
	cfg.Info.CollectorVersion = "0.3.0"
	cfg.RescanInterval = 20 * time.Millisecond
	cfg.CheckVersion = sh.check
	done := f.run(cfg)
	waitFor(t, 5*time.Second, "several version checks", func() bool { return sh.calls.Load() >= 3 })
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) with no new version", err)
	default:
	}
}

func TestFailingShimWarnsAndKeepsRunning(t *testing.T) {
	f := newFixture(t)
	sh := newShim("0.3.0")
	sh.fail.Store(true)
	cfg := f.config()
	cfg.Info.CollectorVersion = "0.3.0"
	cfg.RescanInterval = 20 * time.Millisecond
	cfg.CheckVersion = sh.check
	done := f.run(cfg)
	waitFor(t, 5*time.Second, "a retry on the next rescan", func() bool { return sh.calls.Load() >= 2 })

	f.write(key, "{\"n\":1}\n")
	waitFor(t, 5*time.Second, "a change to ship", func() bool { return f.hubHas(key) })
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) after a failing shim", err)
	default:
	}
}

func TestHungShimTimesOut(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	cfg := f.config()
	cfg.Info.CollectorVersion = "0.3.0"
	cfg.RescanInterval = 20 * time.Millisecond
	cfg.VersionTimeout = 50 * time.Millisecond
	cfg.CheckVersion = func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	f.run(cfg)
	waitFor(t, 5*time.Second, "a check after the first timed out", func() bool { return calls.Load() >= 2 })
}

func TestOnRescanRunsEachRescan(t *testing.T) {
	f := newFixture(t)
	var n atomic.Int32
	cfg := f.config()
	cfg.RescanInterval = 20 * time.Millisecond
	cfg.OnRescan = func() { n.Add(1) }
	f.run(cfg)
	waitFor(t, 5*time.Second, "several rescans", func() bool { return n.Load() >= 3 })
}
