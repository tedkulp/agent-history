package runner

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/reconcile"
	"github.com/tedkulp/agent-history/internal/collector/state"
	"github.com/tedkulp/agent-history/internal/collector/status"
	"github.com/tedkulp/agent-history/protocol"
)

// gaugeHub records the most record uploads it ever saw at once, and every
// PUT /machines/{id} body.
type gaugeHub struct {
	reconcile.Hub
	now, peak atomic.Int32
	mu        sync.Mutex
	puts      []protocol.MachineInfo
}

func (g *gaugeHub) track() func() {
	n := g.now.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	// Hold the slot long enough for an overlapping upload to show.
	time.Sleep(5 * time.Millisecond)
	return func() { g.now.Add(-1) }
}

func (g *gaugeHub) Append(ctx context.Context, source, key string, offset int64, prefix string, data []byte) (protocol.RecordState, error) {
	defer g.track()()
	return g.Hub.Append(ctx, source, key, offset, prefix, data)
}

func (g *gaugeHub) Replace(ctx context.Context, source, key string, data []byte) (protocol.RecordState, error) {
	defer g.track()()
	return g.Hub.Replace(ctx, source, key, data)
}

func (g *gaugeHub) PutMachine(ctx context.Context, info protocol.MachineInfo) error {
	g.mu.Lock()
	g.puts = append(g.puts, info)
	g.mu.Unlock()
	return g.Hub.PutMachine(ctx, info)
}

func (g *gaugeHub) lastPut() protocol.MachineInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.puts[len(g.puts)-1]
}

func (f *fixture) status(c *Control) status.Report {
	f.t.Helper()
	rep, err := c.Status(ctx5s(f.t))
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

func ctx5s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestStatusReportsHubAndSources(t *testing.T) {
	f := newFixture(t)
	f.write(key, cwdLine("/Users/ted/src/app"))
	f.write(proj+"/"+sess2+".jsonl", cwdLine("/Users/ted/src/app"))
	secret := "-Users-ted-src-secret-app/" + sess + ".jsonl"
	f.write(secret, cwdLine("/Users/ted/src/secret/app"))
	cfg := f.config()
	cfg.Exclude = excludeFilter(t)
	cfg.Info.CollectorVersion = "0.3.1"
	cfg.Control = NewControl()
	f.start(cfg)
	var rep status.Report
	waitFor(t, 5*time.Second, "startup reconcile", func() bool {
		var err error
		if rep, err = cfg.Control.Status(ctx5s(t)); err != nil {
			t.Fatal(err)
		}
		return rep.Hub.LastSync != nil
	})
	if !rep.ServiceRunning || rep.Version != "0.3.1" || rep.DisplayName != "test" {
		t.Errorf("collector fields: %+v", rep)
	}
	if rep.Hub.Reachable == nil || !*rep.Hub.Reachable || rep.Hub.LastSync == nil || rep.Hub.LastError != nil {
		t.Errorf("hub: %+v", rep.Hub)
	}
	if rep.Pending == nil || *rep.Pending != 0 {
		t.Errorf("pending = %v", rep.Pending)
	}
	if len(rep.Sources) != 1 {
		t.Fatalf("sources: %+v", rep.Sources)
	}
	s := rep.Sources[0]
	if s.ID != "claude-code" || s.Root != f.root || !s.Enabled || !s.Detected || strings.Join(s.Layouts, ",") != "jsonl" ||
		s.Records != 3 || s.Excluded == nil || *s.Excluded != 1 || s.LastError != nil {
		t.Errorf("source: %+v", s)
	}
}

func TestStatusReportsUnreachableHub(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.down.Store(true)
	cfg := f.config()
	cfg.Control = NewControl()
	f.start(cfg)

	var reachable *bool
	waitFor(t, 5*time.Second, "a failed reconcile", func() bool {
		rep, err := cfg.Control.Status(ctx5s(t))
		if err != nil {
			t.Fatal(err)
		}
		reachable = rep.Hub.Reachable
		return reachable != nil && rep.Hub.LastError != nil
	})
	if *reachable {
		t.Error("hub reported reachable")
	}
}

func TestSyncThroughControlShipsChanges(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.NoWatch = true // only the sync can find the change
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	f.appendLine(key, "{\"n\":2}")
	var lines []string
	res, err := cfg.Control.Sync(ctx5s(t), func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || !f.hubHas(key) {
		t.Errorf("result %+v, hub has change: %v", res, f.hubHas(key))
	}
	if len(lines) == 0 || lines[len(lines)-1] != "1/1 records" {
		t.Errorf("progress = %q", lines)
	}
}

func TestSyncRetriesWithoutWaitingOutBackoff(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.down.Store(true)
	cfg := f.config()
	cfg.BackoffMin, cfg.BackoffMax = time.Hour, time.Hour
	cfg.Control = NewControl()
	f.start(cfg)

	if _, err := cfg.Control.Sync(ctx5s(t), func(string) {}); err == nil {
		t.Fatal("sync with the Hub down succeeded")
	}
	f.down.Store(false)
	if _, err := cfg.Control.Sync(ctx5s(t), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if !f.hubHas(key) {
		t.Error("record not on the Hub after sync")
	}
}

func TestConcurrentSyncsNeverUploadAtOnce(t *testing.T) {
	f := newFixture(t)
	for i := range 20 {
		f.write(proj+"/"+sess[:len(sess)-2]+string(rune('a'+i))+"0.jsonl", "{\"n\":1}\n")
	}
	g := &gaugeHub{Hub: f.hub}
	cfg := f.config()
	cfg.Hub = g
	cfg.Control = NewControl()
	f.start(cfg)

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cfg.Control.Sync(ctx5s(t), func(string) {})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := g.peak.Load(); p != 1 {
		t.Errorf("%d uploads at once, want 1", p)
	}
}

func TestOnceReconcilesAndStops(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	unlock, err := state.Lock(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	var lines []string
	res, err := Once(ctx5s(t), f.config(), func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || !f.hubHas(key) {
		t.Errorf("result %+v", res)
	}
	if len(lines) == 0 {
		t.Error("no progress")
	}
	// A second sync can't start while this one holds the lock.
	if _, err := state.Lock(f.state); !errors.Is(err, state.ErrLocked) {
		t.Errorf("second lock: err = %v, want ErrLocked", err)
	}
}

func TestOnceFailsWhenHubIsDown(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.down.Store(true)
	if _, err := Once(ctx5s(t), f.config(), func(string) {}); err == nil {
		t.Fatal("Once succeeded with the Hub down")
	}
}

func TestSetNameSendsItToTheHub(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	g := &gaugeHub{Hub: f.hub}
	cfg := f.config()
	cfg.Hub = g
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	if err := cfg.Control.SetName(ctx5s(t), "renamed"); err != nil {
		t.Fatal(err)
	}
	put := g.lastPut()
	if put.DisplayName != "renamed" || len(put.Sources) != 1 || !put.Sources[0].Detected {
		t.Errorf("PUT /machines body: %+v", put)
	}
	rep, err := cfg.Control.Status(ctx5s(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.DisplayName != "renamed" {
		t.Errorf("status display name = %q", rep.DisplayName)
	}
	// Later reconciles keep the new name.
	if _, err := cfg.Control.Sync(ctx5s(t), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if put := g.lastPut(); put.DisplayName != "renamed" {
		t.Errorf("reconcile sent display name %q", put.DisplayName)
	}
}

func TestControlAfterRunReturns(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Control = NewControl()
	stop := f.start(cfg)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Control.Status(ctx5s(t)); !errors.Is(err, ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}

func TestSlowSyncStillGetsTheLastProgressLine(t *testing.T) {
	w := newSyncWaiter()
	for i := range 100 {
		w.say(strconv.Itoa(i))
	}
	w.done <- reconciled{}
	var last string
	if _, err := w.wait(ctx5s(t), func(s string) { last = s }); err != nil {
		t.Fatal(err)
	}
	if last != "99" {
		t.Errorf("last line = %q, want 99", last)
	}
}

func TestSourceErrorClearsOnceARecordShips(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.Debounce = 300 * time.Millisecond
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	// A record that turns unreadable before it ships gives the Source an error...
	f.appendLine(key, "{\"n\":2}")
	if err := os.Chmod(f.path(key), 0); err != nil {
		t.Fatal(err)
	}
	lastErr := func() *status.Failure {
		rep, err := cfg.Control.Status(ctx5s(t))
		if err != nil {
			t.Fatal(err)
		}
		return rep.Sources[0].LastError
	}
	waitFor(t, 5*time.Second, "a source error", func() bool { return lastErr() != nil })

	// ...until it ships again.
	if err := os.Chmod(f.path(key), 0o644); err != nil {
		t.Fatal(err)
	}
	f.appendLine(key, "{\"n\":3}")
	waitFor(t, 5*time.Second, "the source error to clear", func() bool { return lastErr() == nil })
}
