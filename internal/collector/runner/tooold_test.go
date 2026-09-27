package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/status"
)

func TestTooOldAtStartStopsAndRetriesHourly(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.tooOld.Store(true)
	cfg := f.config()
	cfg.Control = NewControl()
	f.start(cfg)

	var rep status.Report
	waitFor(t, 5*time.Second, "the 426", func() bool {
		rep = f.status(cfg.Control)
		return rep.Hub.UpgradeRequired
	})
	if rep.Hub.MinCollectorVersion != "0.5.0" || rep.Hub.Reachable == nil || !*rep.Hub.Reachable {
		t.Fatalf("hub: %+v", rep.Hub)
	}
	// Far longer than the outage backoff: a 426 waits an hour, not seconds.
	time.Sleep(300 * time.Millisecond)
	if n := f.refused.Load(); n != 1 {
		t.Fatalf("%d requests refused, want only the first PUT", n)
	}
	if f.onHub(key) {
		t.Fatal("record uploaded despite the 426")
	}
}

func TestTooOldRecoversAfterHourlyRetry(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.Control = NewControl()
	cfg.UpgradeRetry = 100 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "the startup reconcile", func() bool { return f.hubHas(key) })

	// The Hub raises its floor; the next live upload gets a 426.
	f.tooOld.Store(true)
	f.appendLine(key, `{"n":2}`)
	waitFor(t, 5*time.Second, "the 426", func() bool { return f.status(cfg.Control).Hub.UpgradeRequired })
	if f.hubHas(key) {
		t.Fatal("record shipped despite the 426")
	}

	// Floor lowered (or Collector upgraded): the retry's PUT succeeds and
	// a reconcile ships what was held back.
	f.tooOld.Store(false)
	waitFor(t, 5*time.Second, "the reconcile after the retry", func() bool { return f.hubHas(key) })
	rep := f.status(cfg.Control)
	if rep.Hub.UpgradeRequired || rep.Hub.Reachable == nil || !*rep.Hub.Reachable {
		t.Fatalf("hub after recovery: %+v", rep.Hub)
	}
}

func TestSyncWhileTooOldReportsIt(t *testing.T) {
	f := newFixture(t)
	f.tooOld.Store(true)
	cfg := f.config()
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the 426", func() bool { return f.status(cfg.Control).Hub.UpgradeRequired })

	_, err := cfg.Control.Sync(ctx5s(t), func(string) {})
	var tooOld *hubclient.TooOldError
	if !errors.As(err, &tooOld) || tooOld.MinVersion != "0.5.0" {
		t.Fatalf("sync err = %v, want TooOldError", err)
	}
}

func TestOutageWhileTooOldKeepsHourlyRetry(t *testing.T) {
	f := newFixture(t)
	f.tooOld.Store(true)
	cfg := f.config()
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the 426", func() bool { return f.status(cfg.Control).Hub.UpgradeRequired })

	// A sync retries at once and hits an outage.
	f.down.Store(true)
	if _, err := cfg.Control.Sync(ctx5s(t), func(string) {}); !hubclient.Transient(err) {
		t.Fatalf("sync err = %v, want a transient error", err)
	}
	before := f.requests.Load()
	// Far longer than the outage backoff (BackoffMin 20ms).
	time.Sleep(300 * time.Millisecond)
	if n := f.requests.Load() - before; n != 0 {
		t.Fatalf("%d requests after the failed retry, want none before the hour is up", n)
	}
	if !f.status(cfg.Control).Hub.UpgradeRequired {
		t.Fatal("upgrade line dropped after an outage")
	}
}
