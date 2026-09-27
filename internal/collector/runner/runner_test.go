package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/api"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	machine = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"
	sess    = "5f1c9a2e-8d1b-4f7a-9c2e-5b0d7e1a90e2"
	sess2   = "6a2d0b3f-9e2c-4a8b-8d3f-6c1e8f2b01f3"
	proj    = "-Users-ted-src-app"
	key     = proj + "/" + sess + ".jsonl"
)

// fixture is a real Hub behind a switchable front door, plus a Claude Code
// root on disk.
type fixture struct {
	t     *testing.T
	store *store.Store
	hub   *hubclient.Client
	root  string
	state string

	down     atomic.Bool  // every request fails with a dropped connection
	dropAcks atomic.Int32 // this many records requests commit, then drop the connection
	posts    atomic.Int32 // records requests that reached the Hub
	requests atomic.Int32 // every request, including refused ones
	blockMu  sync.Mutex
	block    chan struct{} // when set, records requests wait for it to close
	blocked  chan struct{} // receives once per request that starts waiting
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, store: st, root: t.TempDir(), state: t.TempDir(), blocked: make(chan struct{}, 16)}
	h := api.New(st, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.down.Load() {
			hangUp(w)
			return
		}
		if r.Method != http.MethodPost {
			h.ServeHTTP(w, r)
			return
		}
		f.blockMu.Lock()
		block := f.block
		f.blockMu.Unlock()
		if block != nil {
			// Read the body first, so the handler notices when the Collector hangs up.
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			f.blocked <- struct{}{}
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		f.posts.Add(1)
		if f.dropAcks.Load() > 0 {
			f.dropAcks.Add(-1)
			h.ServeHTTP(httptest.NewRecorder(), r)
			hangUp(w)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.hub, err = hubclient.New(srv.URL, machine, "0.0.0-dev", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// hangUp drops the connection without a response, as a killed Hub would.
func hangUp(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		conn.Close()
	}
}

func (f *fixture) setBlock(c chan struct{}) {
	f.blockMu.Lock()
	f.block = c
	f.blockMu.Unlock()
}

func (f *fixture) path(rel string) string { return filepath.Join(f.root, filepath.FromSlash(rel)) }

func (f *fixture) write(rel, body string) {
	f.t.Helper()
	p := f.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appendLine(rel, line string) {
	f.t.Helper()
	fh, err := os.OpenFile(f.path(rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if _, err := io.WriteString(fh, line+"\n"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) cachePath() string { return filepath.Join(f.state, "cache.json") }

// config returns a Config with short timings; tests override fields.
func (f *fixture) config() Config {
	return Config{
		Hub:            f.hub,
		Info:           protocol.MachineInfo{DisplayName: "test", HomeDir: "/Users/ted"},
		Sources:        []Source{{Adapter: claudecode.Adapter{}, Root: f.root}},
		Cache:          cache.Load(f.cachePath(), "hub", nil),
		Log:            slog.New(slog.NewTextHandler(testWriter{f.t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		RescanInterval: time.Hour,
		Debounce:       20 * time.Millisecond,
		DebounceCap:    200 * time.Millisecond,
		CacheFlush:     50 * time.Millisecond,
		DrainTimeout:   5 * time.Second,
		BackoffMin:     20 * time.Millisecond,
		BackoffMax:     200 * time.Millisecond,
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// start runs the Collector until the returned stop func is called (or the
// test ends). stop returns Run's error.
func (f *fixture) start(cfg Config) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				f.t.Fatal("Run did not return after cancel")
			}
		})
		return err
	}
	f.t.Cleanup(func() { stop() })
	return stop
}

// hubHas reports whether the Hub's current version of rel matches disk.
func (f *fixture) hubHas(rel string) bool {
	b, err := os.ReadFile(f.path(rel))
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := f.store.Manifest(context.Background(), machine, "")
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	for _, r := range m {
		if r.RecordKey == rel {
			return r.Length == int64(len(b)) && r.Sha256 == hex.EncodeToString(sum[:])
		}
	}
	return false
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

func TestLiveChangeReachesHubWithinSeconds(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.Debounce, cfg.DebounceCap = 0, 0 // the spec's 2 s / 30 s
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	f.appendLine(key, "{\"n\":2}")
	took := waitFor(t, 5*time.Second, "the new message on the Hub", func() bool { return f.hubHas(key) })
	t.Logf("new message reached the Hub in %v", took)

	// A new Session in a new project directory is watched too.
	other := "-Users-ted-src-new/" + sess2 + ".jsonl"
	f.write(other, "{\"n\":1}\n")
	waitFor(t, 5*time.Second, "a Session in a new directory", func() bool { return f.hubHas(other) })
}

func TestRescanPicksUpChangeWithoutWatch(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.NoWatch = true
	cfg.RescanInterval = 100 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	f.appendLine(key, "{\"n\":2}")
	waitFor(t, 5*time.Second, "the rescan to ship the change", func() bool { return f.hubHas(key) })
}

func TestDebounceCoalescesAndCapsBursts(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":0}\n")
	cfg := f.config()
	cfg.Debounce = 100 * time.Millisecond
	cfg.DebounceCap = 400 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	// A write every 20 ms for 1.2 s: the debounce never settles, so only
	// the 400 ms cap ships during the burst.
	before := f.posts.Load()
	sawMidBurst := false
	for i := 1; i <= 60; i++ {
		f.appendLine(key, "{\"n\":1}")
		time.Sleep(20 * time.Millisecond)
		if f.posts.Load() > before {
			sawMidBurst = true
		}
	}
	waitFor(t, 5*time.Second, "the burst on the Hub", func() bool { return f.hubHas(key) })
	if !sawMidBurst {
		t.Fatal("nothing shipped during a 1.2 s burst; the 400 ms cap should have fired")
	}
	if n := f.posts.Load() - before; n > 8 {
		t.Fatalf("%d uploads for 60 writes; the debounce should coalesce them", n)
	}
}

func TestHubOutageBacksOffAndReconciles(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.start(f.config())
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	// The Hub is killed mid-upload: it commits the append, but the ack is lost.
	f.dropAcks.Store(1)
	f.appendLine(key, "{\"n\":2}")
	waitFor(t, 5*time.Second, "the lost ack", func() bool { return f.dropAcks.Load() == 0 })

	// Then it stays down while the Session keeps growing.
	f.down.Store(true)
	f.appendLine(key, "{\"n\":3}")
	f.write(proj+"/"+sess2+".jsonl", "{\"n\":1}\n")
	time.Sleep(600 * time.Millisecond)
	// With a 20 ms floor doubling to 200 ms, 600 ms of outage allows only a
	// handful of attempts; a retry loop without backoff would make dozens.
	if n := f.requests.Load(); n > 40 {
		t.Fatalf("%d requests during the outage; want backoff", n)
	}

	f.down.Store(false)
	waitFor(t, 5*time.Second, "the Hub's manifest to match disk", func() bool {
		return f.hubHas(key) && f.hubHas(proj+"/"+sess2+".jsonl")
	})
}

func TestStartsWhileHubIsDown(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	f.down.Store(true)
	f.start(f.config())
	time.Sleep(200 * time.Millisecond)
	f.down.Store(false)
	waitFor(t, 5*time.Second, "the startup reconcile to succeed once the Hub is up", func() bool { return f.hubHas(key) })
}

func TestLostStateDirUploadsNothingHubHas(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n{\"n\":2}\n")
	f.write(proj+"/"+sess+"/subagents/agent-a1.jsonl", "{\"n\":1}\n")
	stop := f.start(f.config())
	waitFor(t, 5*time.Second, "first run to ship", func() bool {
		return f.hubHas(key) && f.hubHas(proj+"/"+sess+"/subagents/agent-a1.jsonl")
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(f.state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	before := f.posts.Load()
	cfg := f.config()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the reconcile to rebuild the cache", func() bool {
		_, ok := cfg.Cache.Get(cache.Key(protocol.SourceClaudeCode, key))
		_, ok2 := cfg.Cache.Get(cache.Key(protocol.SourceClaudeCode, proj+"/"+sess+"/subagents/agent-a1.jsonl"))
		return ok && ok2
	})
	if n := f.posts.Load() - before; n != 0 {
		t.Fatalf("%d uploads after losing the state dir, want 0", n)
	}
}

func TestShutdownDrainsUploadAndWritesCache(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	stop := f.start(f.config())
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	release := make(chan struct{})
	f.setBlock(release)
	f.appendLine(key, "{\"n\":2}")
	select {
	case <-f.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never started")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Run returned (%v) before the in-flight upload finished", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	if !f.hubHas(key) {
		t.Fatal("the drained upload didn't reach the Hub")
	}
	e, ok := cache.Load(f.cachePath(), "hub", nil).Get(cache.Key(protocol.SourceClaudeCode, key))
	if !ok || e.Length != int64(len("{\"n\":1}\n{\"n\":2}\n")) {
		t.Fatalf("cache on disk has %+v (ok %v), want the drained upload's state", e, ok)
	}
}

func TestShutdownGivesUpAfterDrainTimeout(t *testing.T) {
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.DrainTimeout = 200 * time.Millisecond
	stop := f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	f.setBlock(never)
	f.appendLine(key, "{\"n\":2}")
	<-f.blocked
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("shutdown took %v with a 200 ms drain timeout", took)
	}
	if _, err := os.Stat(f.cachePath()); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestSteadyStateSkipsUnchangedFilesByStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	cfg := f.config()
	cfg.RescanInterval = 50 * time.Millisecond
	var logs strings.Builder
	var mu sync.Mutex
	cfg.Log = slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) })

	// Unreadable, but with the same size and mtime: any read would fail.
	if err := os.Chmod(f.path(key), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.path(key), 0o644) })
	before := f.posts.Load()
	time.Sleep(300 * time.Millisecond) // several rescans

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), "permission denied") || f.posts.Load() != before {
		t.Fatalf("an unchanged file was read or shipped:\n%s", logs.String())
	}
}

func TestShipSkipsByStatWithoutReading(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	f := newFixture(t)
	f.write(key, "{\"n\":1}\n")
	fi, err := os.Stat(f.path(key))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.path(key), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(f.path(key), 0o644) })
	cfg := f.config()
	cfg.setDefaults()
	k := cache.Key(protocol.SourceClaudeCode, key)
	cfg.Cache.Set(k, cache.Entry{Length: 8, Sha256: "x", SrcSize: fi.Size(), SrcMtime: fi.ModTime()})
	r := &runner{Config: cfg}
	rec, _ := claudecode.Adapter{}.Layouts()[0].Claims(f.root, f.path(key))
	res := r.ship(context.Background(), job{key: k, src: protocol.SourceClaudeCode, rec: rec})
	if res.unreadable || res.failedAt != nil || res.transient || f.requests.Load() != 0 {
		t.Fatalf("result %+v, %d requests: the file should be skipped by stat", res, f.requests.Load())
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func TestRescanDropsCacheEntriesForDeletedRecords(t *testing.T) {
	f := newFixture(t)
	gone := proj + "/" + sess2 + ".jsonl"
	f.write(key, "{\"n\":1}\n")
	f.write(gone, "{\"n\":1}\n")
	cfg := f.config()
	cfg.RescanInterval = 50 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "startup reconcile", func() bool { return f.hubHas(key) && f.hubHas(gone) })
	if err := os.Remove(f.path(gone)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the rescan to drop the deleted record", func() bool {
		_, ok := cfg.Cache.Get(cache.Key(protocol.SourceClaudeCode, gone))
		return !ok
	})
	if _, ok := cfg.Cache.Get(cache.Key(protocol.SourceClaudeCode, key)); !ok {
		t.Fatal("the rescan dropped a record still on disk")
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := time.Second, 5*time.Minute
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		for range 50 {
			if d := backoff(attempt, lo, hi); d < want/2 || d > want {
				t.Fatalf("backoff(%d) = %v, want in [%v, %v]", attempt, d, want/2, want)
			}
		}
	}
	if d := backoff(40, lo, hi); d < hi/2 || d > hi {
		t.Fatalf("backoff(40) = %v, want capped at %v", d, hi)
	}
}

// onHub reports whether the Hub holds any version of rel.
func (f *fixture) onHub(rel string) bool {
	m, err := f.store.Manifest(context.Background(), machine, "")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range m {
		if r.RecordKey == rel {
			return true
		}
	}
	return false
}

func cwdLine(cwd string) string { return `{"type":"user","cwd":"` + cwd + `"}` + "\n" }

func excludeFilter(t *testing.T) *exclude.Filter {
	t.Helper()
	filter, err := exclude.New([]string{"/Users/ted/src/secret/**"})
	if err != nil {
		t.Fatal(err)
	}
	return filter
}

func TestExcludedSessionNeverLeavesMachine(t *testing.T) {
	f := newFixture(t)
	secretProj := "-Users-ted-src-secret-app"
	secret := secretProj + "/" + sess + ".jsonl"
	secretFiles := []string{
		secret,
		secretProj + "/" + sess + "/subagents/agent-a1.jsonl",
		secretProj + "/" + sess + "/subagents/agent-a1.meta.json",
		secretProj + "/" + sess + "/tool-results/toolu_01.txt",
		secretProj + "/" + sess + "/custom-title.json",
	}
	open := proj + "/" + sess2 + ".jsonl"
	openChild := proj + "/" + sess2 + "/subagents/agent-b1.jsonl"
	f.write(secret, cwdLine("/Users/ted/src/secret/app"))
	for _, k := range secretFiles[1:] {
		// A Child Session's own cwd doesn't save it.
		f.write(k, cwdLine("/Users/ted/src/app"))
	}
	f.write(open, cwdLine("/Users/ted/src/app"))
	f.write(openChild, cwdLine("/Users/ted/src/secret/app"))

	cfg := f.config()
	cfg.Exclude = excludeFilter(t)
	cfg.RescanInterval = 50 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "the open Session to ship", func() bool { return f.hubHas(open) && f.hubHas(openChild) })

	// Live changes to the excluded Session, and a new excluded Session.
	f.appendLine(secret, `{"n":2}`)
	f.appendLine(secretFiles[1], `{"n":2}`)
	newSecret := "-Users-ted-src-secret-other/" + sess2 + ".jsonl"
	f.write(newSecret, cwdLine("/Users/ted/src/secret/other"))
	f.appendLine(open, `{"n":2}`)
	waitFor(t, 5*time.Second, "the open Session's change to ship", func() bool { return f.hubHas(open) })
	time.Sleep(200 * time.Millisecond) // a few rescans and debounces

	for _, k := range append(secretFiles, newSecret) {
		if f.onHub(k) {
			t.Errorf("excluded record %s reached the Hub", k)
		}
	}
	if n := cfg.Exclude.Count(protocol.SourceClaudeCode); n != len(secretFiles)+1 {
		t.Errorf("excluded count = %d, want %d", n, len(secretFiles)+1)
	}
}

func TestEmptySessionWaitsForCwd(t *testing.T) {
	for _, c := range []struct {
		name       string
		cwd        string
		ships      bool
		rescanOnly bool
	}{
		{"not excluded, watched", "/Users/ted/src/app", true, false},
		{"excluded, watched", "/Users/ted/src/secret/app", false, false},
		{"not excluded, rescan only", "/Users/ted/src/app", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			attach := proj + "/" + sess + "/tool-results/toolu_01.txt"
			f.write(key, "")
			f.write(attach, "result\n")
			cfg := f.config()
			cfg.Exclude = excludeFilter(t)
			if c.rescanOnly {
				cfg.NoWatch = true
				cfg.RescanInterval = 50 * time.Millisecond
			}
			f.start(cfg)
			marker := proj + "/" + sess2 + ".jsonl"
			f.write(marker, cwdLine("/Users/ted/src/app"))
			waitFor(t, 5*time.Second, "another Session to ship", func() bool { return f.hubHas(marker) })
			time.Sleep(100 * time.Millisecond)
			if f.onHub(key) || f.onHub(attach) {
				t.Fatal("a Session was shipped before its cwd was readable")
			}

			f.appendLine(key, strings.TrimSuffix(cwdLine(c.cwd), "\n"))
			if c.ships {
				waitFor(t, 5*time.Second, "the Session and its attachment to ship", func() bool { return f.hubHas(key) && f.hubHas(attach) })
				return
			}
			time.Sleep(300 * time.Millisecond)
			if f.onHub(key) || f.onHub(attach) {
				t.Fatal("an excluded Session was shipped once its cwd was readable")
			}
		})
	}
}
