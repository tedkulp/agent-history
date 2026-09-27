// Package runner is the Collector's long-running loop (collector.md §4.2):
// the startup reconcile, the fsnotify watch, the periodic rescan, debounced
// uploads, backoff and reconcile after a Hub outage, and a draining shutdown.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/reconcile"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/status"
	"github.com/tedkulp/agent-history/protocol"
)

// Source is one enabled Source and its configured root.
type Source struct {
	Adapter source.Adapter
	Root    string
}

// Config configures Run. Zero timings take the spec's values; tests
// shorten them.
type Config struct {
	Hub     reconcile.Hub
	Info    protocol.MachineInfo // Sources is filled in from detection
	Sources []Source
	Cache   *cache.Cache
	Log     *slog.Logger
	// Exclude holds back Sessions by starting cwd (collector.md §4.6).
	// Nil excludes nothing.
	Exclude *exclude.Filter

	RescanInterval time.Duration // 10m
	// NoWatch turns off fsnotify, leaving the rescan to find changes.
	NoWatch bool
	// Control, when set, lets other goroutines ask Run for its status, a
	// sync or a rename.
	Control *Control

	Debounce     time.Duration // 2s after a record's last event
	DebounceCap  time.Duration // 30s: a record that keeps changing still ships this often
	CacheFlush   time.Duration // 5s: the cache is written at most this often
	DrainTimeout time.Duration // 10s for in-flight uploads on shutdown
	BackoffMin   time.Duration // 1s, doubling after each failed reconcile
	BackoffMax   time.Duration // 5m
	UpgradeRetry time.Duration // 1h between retries after a 426 (protocol.md §4.6)
	MaxUploads   int           // 4 in flight across all records

	// firstSync waits on the startup reconcile (see Once).
	firstSync *syncWaiter
}

func (c *Config) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.RescanInterval, 10*time.Minute)
	def(&c.Debounce, 2*time.Second)
	def(&c.DebounceCap, 30*time.Second)
	def(&c.CacheFlush, 5*time.Second)
	def(&c.DrainTimeout, 10*time.Second)
	def(&c.BackoffMin, time.Second)
	def(&c.BackoffMax, 5*time.Minute)
	def(&c.UpgradeRetry, time.Hour)
	if c.MaxUploads == 0 {
		c.MaxUploads = 4
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// pending is a dirty record waiting out its debounce.
type pending struct {
	src         string
	rec         source.Record
	first, last time.Time
}

// waiter is a record whose exclude decision waits for its Session's cwd
// to become readable.
type waiter struct {
	src    Source
	layout source.Layout
	rec    source.Record
}

// fileStat is the change signal of a record's file.
type fileStat struct {
	size  int64
	mtime time.Time
}

func statOf(fi fs.FileInfo) fileStat { return fileStat{fi.Size(), fi.ModTime()} }

// job is one record handed to an upload goroutine.
type job struct {
	key      string
	src      string
	rec      source.Record
	failedAt *fileStat // the stat at the record's last non-transient failure
	warned   bool      // an unreadable-file warning was already logged
}

// result is what an upload goroutine reports back.
type result struct {
	key        string
	acked      bool      // the Hub acknowledged the record's state
	err        error     // why the record wasn't shipped, if it failed
	failedAt   *fileStat // skip the record until its stat differs from this
	unreadable bool      // the file couldn't be read; the next rescan retries it
	transient  bool      // the Hub is unreachable
	tooOld     *hubclient.TooOldError
}

type reconciled struct {
	res reconcile.Result
	err error
}

// runner's fields are owned by the Run goroutine; upload and reconcile
// goroutines talk to it only through the results and reconciled channels.
type runner struct {
	Config

	detected    []Source
	wasDetected map[string]bool

	dirty      map[string]*pending
	inFlight   map[string]bool
	failed     map[string]fileStat
	unreadable map[string]bool
	waiting    map[string]waiter

	online        bool
	needReconcile bool
	reconciling   bool
	attempt       int
	nextReconcile time.Time
	lastSave      time.Time

	watcher     *fsnotify.Watcher
	watched     map[string]bool
	watchWarned map[string]bool

	// What `status` reports.
	infos   map[string]protocol.SourceInfo // from the last discover
	records map[string]int                 // every discovered record, per Source
	// A Source's last error: from discovery until it next succeeds, from
	// an upload until one of its records next ships.
	discoverErr map[string]*status.Failure
	uploadErr   map[string]*status.Failure
	contacted   bool // a reconcile has reached the Hub or failed to
	lastSync    time.Time
	hubErr      *status.Failure
	// tooOld is the Hub's last 426, until a reconcile succeeds. Nothing
	// ships meanwhile; a reconcile is retried every UpgradeRetry.
	tooOld *hubclient.TooOldError

	// Syncs waiting for the next reconcile, and those the running one will answer.
	syncQueued  []*syncWaiter
	syncRunning []*syncWaiter

	results    chan result
	reconciled chan reconciled
}

// Run ships changes to the Hub until ctx is cancelled, then drains in-flight
// uploads for up to DrainTimeout, writes the cache and returns nil.
func Run(ctx context.Context, cfg Config) error {
	cfg.setDefaults()
	r := &runner{
		Config:        cfg,
		wasDetected:   map[string]bool{},
		dirty:         map[string]*pending{},
		inFlight:      map[string]bool{},
		failed:        map[string]fileStat{},
		unreadable:    map[string]bool{},
		waiting:       map[string]waiter{},
		needReconcile: true,
		watched:       map[string]bool{},
		watchWarned:   map[string]bool{},
		infos:         map[string]protocol.SourceInfo{},
		records:       map[string]int{},
		discoverErr:   map[string]*status.Failure{},
		uploadErr:     map[string]*status.Failure{},
		results:       make(chan result, cfg.MaxUploads),
		reconciled:    make(chan reconciled, 1),
		lastSave:      time.Now(),
	}
	if cfg.firstSync != nil {
		r.syncQueued = append(r.syncQueued, cfg.firstSync)
	}
	var controls <-chan func(*runner)
	if cfg.Control != nil {
		controls = cfg.Control.reqs
		defer close(cfg.Control.stopped)
	}
	if !cfg.NoWatch {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			r.Log.Warn("file watching unavailable, relying on the rescan", "err", err)
		} else {
			r.watcher = w
			defer w.Close()
		}
	}
	var events <-chan fsnotify.Event
	var watchErrs <-chan error
	if r.watcher != nil {
		events, watchErrs = r.watcher.Events, r.watcher.Errors
	}

	// Uploads outlive ctx so that shutdown can drain them; cancelUploads
	// ends them once the drain times out.
	uploadCtx, cancelUploads := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelUploads()

	r.discover()
	r.updateWatches()

	rescan := time.NewTicker(r.RescanInterval)
	defer rescan.Stop()
	wake := time.NewTimer(0)
	defer wake.Stop()

	for {
		select {
		case <-ctx.Done():
			return r.shutdown(cancelUploads)
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			r.handleEvent(ev)
		case err, ok := <-watchErrs:
			if !ok {
				watchErrs = nil
				continue
			}
			r.Log.Warn("file watch error, relying on the rescan", "err", err)
		case res := <-r.results:
			r.handleResult(res)
		case rc := <-r.reconciled:
			r.handleReconciled(rc)
		case fn := <-controls:
			fn(r)
		case <-rescan.C:
			r.rescan()
		case <-wake.C:
		}
		if next := r.step(ctx, uploadCtx, time.Now()); next.IsZero() {
			wake.Stop()
		} else {
			wake.Reset(time.Until(next))
		}
	}
}

// step starts whatever work is due and returns when it next needs to run
// (zero when only an event can make more work due).
func (r *runner) step(ctx, uploadCtx context.Context, now time.Time) time.Time {
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}

	if r.needReconcile && !r.reconciling && len(r.inFlight) == 0 {
		if now.Before(r.nextReconcile) {
			soonest(r.nextReconcile)
		} else {
			r.startReconcile(ctx, uploadCtx)
		}
	}

	if r.online && !r.needReconcile && !r.reconciling {
		for k, p := range r.dirty {
			if r.inFlight[k] {
				continue
			}
			due := p.last.Add(r.Debounce)
			if capped := p.first.Add(r.DebounceCap); capped.Before(due) {
				due = capped
			}
			if now.Before(due) {
				soonest(due)
				continue
			}
			if len(r.inFlight) >= r.MaxUploads {
				continue
			}
			delete(r.dirty, k)
			r.inFlight[k] = true
			j := job{key: k, src: p.src, rec: p.rec, warned: r.unreadable[k]}
			if f, ok := r.failed[k]; ok {
				j.failedAt = &f
			}
			go func() { r.results <- r.ship(uploadCtx, j) }()
		}
	}

	if r.Cache.Dirty() {
		if due := r.lastSave.Add(r.CacheFlush); now.Before(due) {
			soonest(due)
		} else {
			r.saveCache()
		}
	}
	return next
}

// ship brings one dirty record up to date against the cache. It runs on its
// own goroutine and touches only the cache, which is safe for concurrent use.
func (r *runner) ship(ctx context.Context, j job) result {
	res := result{key: j.key}
	log := r.Log.With("source", j.src, "key", j.rec.Key)
	fi, err := os.Stat(j.rec.Path)
	if errors.Is(err, fs.ErrNotExist) {
		// Gone from disk: the next rescan drops its cache entry.
		return res
	}
	if err != nil {
		if !j.warned {
			log.Warn("can't stat record, retrying on the next rescan", "err", err)
		}
		res.unreadable = true
		res.err = err
		return res
	}
	st := statOf(fi)
	e, cached := r.Cache.Get(j.key)
	if cached && e.Unchanged(fi) {
		return res
	}
	if j.failedAt != nil && *j.failedAt == st {
		return res
	}
	var h *protocol.ManifestRecord
	if cached {
		h = &protocol.ManifestRecord{Length: e.Length, Sha256: e.Sha256}
	}
	out, err := reconcile.Ship(ctx, r.Hub, j.src, j.rec, h, log)
	var mismatch *reconcile.MismatchError
	var pathErr *fs.PathError
	var tooOld *hubclient.TooOldError
	switch {
	case err == nil:
		r.Cache.Set(j.key, out.Entry)
		res.acked = true
		if out.Sent > 0 {
			log.Debug("shipped record", "bytes", out.Sent, "length", out.Entry.Length)
		}
	case hubclient.Transient(err):
		log.Warn("hub unreachable", "err", err)
		res.transient = true
		res.err = err
	case errors.As(err, &tooOld):
		// The record is untouched on the Hub; the reconcile after the
		// retry ships it.
		res.tooOld = tooOld
	case errors.Is(err, context.Canceled):
		// Shutdown gave up on the drain; the next start reconciles.
	case errors.As(err, &mismatch):
		log.Warn("hub state differs from local content", "err", err)
		r.Cache.Set(j.key, mismatch.Entry())
	case errors.As(err, &pathErr):
		if !j.warned {
			log.Warn("can't read record, retrying on the next rescan", "err", err)
		}
		res.unreadable = true
		res.err = err
	default:
		log.Error("shipping record, skipping it until it next changes", "err", err)
		res.failedAt = &st
		res.err = err
	}
	return res
}

func (r *runner) handleResult(res result) {
	delete(r.inFlight, res.key)
	now := time.Now()
	src, _ := cache.Split(res.key)
	switch {
	case res.acked:
		r.lastSync = now
		r.hubErr = nil
		delete(r.uploadErr, src)
	case res.transient:
		r.hubErr = status.NewFailure(res.err, now)
	case res.tooOld != nil && !r.needReconcile:
		// Other in-flight uploads may get the same 426; the first one
		// decides.
		r.upgradeRequired(res.tooOld)
	case res.tooOld != nil:
	case res.err != nil:
		r.uploadErr[src] = status.NewFailure(res.err, now)
	}
	if res.failedAt != nil {
		r.failed[res.key] = *res.failedAt
	} else {
		delete(r.failed, res.key)
	}
	if res.unreadable {
		r.unreadable[res.key] = true
	} else {
		delete(r.unreadable, res.key)
	}
	if res.transient && !r.needReconcile {
		// Stop shipping; once in-flight uploads settle, back off and
		// reconcile (protocol.md §4.5). There is no spool: the reconcile
		// finds everything that changed meanwhile on disk.
		r.online = false
		r.needReconcile = true
		r.attempt = 0
		r.scheduleReconcile()
	}
}

func (r *runner) startReconcile(ctx, uploadCtx context.Context) {
	// The reconcile covers every record on disk; events from now on mark
	// records dirty again, as does discover for records it stops waiting on.
	clear(r.dirty)
	sources, infos := r.discover()
	r.updateWatches()
	info := r.Info
	info.Sources = infos
	r.reconciling = true
	waiters := r.syncQueued
	r.syncRunning, r.syncQueued = waiters, nil
	opts := reconcile.Options{Cache: r.Cache, Log: r.Log, Stop: ctx.Done()}
	if len(waiters) > 0 {
		opts.Progress = progressReporter(waiters)
	}
	go func() {
		start := time.Now()
		res, err := reconcile.Reconcile(uploadCtx, r.Hub, info, sources, opts)
		if err == nil {
			r.Log.Info("reconcile finished", "uploaded", res.Uploaded, "unchanged", res.Unchanged, "replaced", res.Replaced,
				"failed", res.Failed, "mismatched", res.Mismatched, "bytes", res.Bytes, "took", time.Since(start).Round(time.Millisecond))
		}
		r.reconciled <- reconciled{res, err}
	}()
}

func (r *runner) handleReconciled(rc reconciled) {
	r.reconciling = false
	for _, w := range r.syncRunning {
		w.done <- rc
	}
	r.syncRunning = nil
	var tooOld *hubclient.TooOldError
	switch {
	case rc.err == nil:
		if r.tooOld != nil {
			r.Log.Info("hub accepts this Collector again", "min_collector_version", r.tooOld.MinVersion)
			r.tooOld = nil
		}
		r.online = true
		r.contacted = true
		r.needReconcile = len(r.syncQueued) > 0
		r.attempt = 0
		r.lastSync = time.Now()
		r.hubErr = nil
	case errors.Is(rc.err, reconcile.ErrStopped), errors.Is(rc.err, context.Canceled):
		// Shutting down.
	case errors.As(rc.err, &tooOld):
		r.upgradeRequired(tooOld)
	case r.tooOld != nil:
		// The hourly retry hit an outage: still too old, as far as we
		// know, so keep to the hourly cadence rather than the backoff.
		r.hubErr = status.NewFailure(rc.err, time.Now())
		r.nextReconcile = time.Now().Add(r.UpgradeRetry)
		r.Log.Warn("retry after 426 failed", "err", rc.err, "retry_in", r.UpgradeRetry)
	default:
		r.contacted = true
		r.hubErr = status.NewFailure(rc.err, time.Now())
		d := r.scheduleReconcile()
		r.Log.Warn("reconcile failed, backing off", "err", rc.err, "retry_in", d.Round(time.Millisecond))
	}
}

// upgradeRequired stops shipping after a 426 and retries after UpgradeRetry
// (protocol.md §4.6). The retry is a full reconcile: it starts with
// PUT /machines/{id} and goes on to reconcile only once that succeeds. A
// sync retries at once.
func (r *runner) upgradeRequired(e *hubclient.TooOldError) {
	if r.tooOld == nil || r.tooOld.MinVersion != e.MinVersion {
		r.Log.Error("hub requires a newer Collector, stopped uploading until it is upgraded (mise upgrade)",
			"min_collector_version", e.MinVersion, "retry_in", r.UpgradeRetry)
	} else {
		r.Log.Info("hub still requires a newer Collector", "min_collector_version", e.MinVersion, "retry_in", r.UpgradeRetry)
	}
	r.tooOld = e
	r.contacted = true
	r.hubErr = nil
	r.online = false
	r.needReconcile = true
	r.attempt = 0
	r.nextReconcile = time.Now().Add(r.UpgradeRetry)
	if len(r.syncQueued) > 0 {
		// A sync asked while this reconcile ran gets its own answer now.
		r.nextReconcile = time.Time{}
	}
}

// scheduleReconcile sets the next reconcile attempt after an exponential,
// jittered backoff: BackoffMin doubling up to BackoffMax.
func (r *runner) scheduleReconcile() time.Duration {
	d := backoff(r.attempt, r.BackoffMin, r.BackoffMax)
	r.attempt++
	r.nextReconcile = time.Now().Add(d)
	return d
}

// backoff is lo·2^attempt capped at hi, jittered down by up to half.
func backoff(attempt int, lo, hi time.Duration) time.Duration {
	d := lo
	for i := 0; i < attempt && d < hi; i++ {
		d *= 2
	}
	d = min(d, hi)
	return d/2 + rand.N(d/2+1)
}

// rescan re-detects every Source, marks each record whose stat differs from
// the cache dirty, and drops cache entries for records gone from disk.
func (r *runner) rescan() {
	sources, _ := r.discover()
	r.updateWatches()
	for _, s := range sources {
		present := make(map[string]bool, len(s.Records))
		// A waiting record may yet ship: keep its cache entry.
		for _, w := range r.waitingIn(s.ID) {
			present[w.rec.Key] = true
		}
		for _, rec := range s.Records {
			present[rec.Key] = true
			fi, err := os.Stat(rec.Path)
			if err != nil {
				continue
			}
			k := cache.Key(s.ID, rec.Key)
			if e, ok := r.Cache.Get(k); ok && e.Unchanged(fi) {
				continue
			}
			if f, ok := r.failed[k]; ok && f == statOf(fi) {
				continue
			}
			r.markDirty(s.ID, rec)
		}
		r.Cache.Prune(s.ID, present)
		gone := func(k string) bool {
			src, rk := cache.Split(k)
			return src == s.ID && !present[rk]
		}
		for k := range r.failed {
			if gone(k) {
				delete(r.failed, k)
			}
		}
		for k := range r.unreadable {
			if gone(k) {
				delete(r.unreadable, k)
			}
		}
	}
}

// discover re-detects every Source and lists the records of those it can
// read. A Source whose discovery fails is left out until the next rescan, so
// its cache entries are kept.
func (r *runner) discover() ([]reconcile.Source, []protocol.SourceInfo) {
	var sources []reconcile.Source
	infos := []protocol.SourceInfo{}
	r.detected = r.detected[:0]
	for _, s := range r.Sources {
		a := s.Adapter
		si := protocol.SourceInfo{Source: a.ID(), Root: s.Root, Layouts: []string{}, Detected: a.Detect(s.Root)}
		if v := a.Version(s.Root); v != "" {
			si.Version = &v
		}
		if si.Detected != r.wasDetected[a.ID()] {
			if si.Detected {
				r.Log.Info("source detected", "source", a.ID(), "root", s.Root)
			} else {
				r.Log.Info("source not detected", "source", a.ID(), "root", s.Root)
			}
			r.wasDetected[a.ID()] = si.Detected
		}
		if si.Detected {
			r.detected = append(r.detected, s)
			src := reconcile.Source{ID: a.ID()}
			ok := true
			present := map[string]bool{}
			for _, l := range a.Layouts() {
				recs, err := l.Discover(s.Root)
				if err != nil {
					r.Log.Error("discovering records", "source", a.ID(), "layout", l.Name(), "err", err)
					r.discoverErr[a.ID()] = status.NewFailure(fmt.Errorf("discovering %s records: %w", l.Name(), err), time.Now())
					ok = false
					break
				}
				si.Layouts = append(si.Layouts, l.Name())
				for _, rec := range recs {
					present[rec.Key] = true
					if r.shippable(s, l, rec) {
						src.Records = append(src.Records, rec)
					}
				}
			}
			if ok {
				r.records[a.ID()] = len(present)
				delete(r.discoverErr, a.ID())
				r.Exclude.Forget(a.ID(), present)
				for k, w := range r.waitingIn(a.ID()) {
					if !present[w.rec.Key] {
						delete(r.waiting, k)
					}
				}
				sources = append(sources, src)
			}
		}
		r.infos[a.ID()] = si
		infos = append(infos, si)
	}
	return sources, infos
}

func (r *runner) markDirty(src string, rec source.Record) {
	k := cache.Key(src, rec.Key)
	now := time.Now()
	if p, ok := r.dirty[k]; ok {
		p.last = now
		return
	}
	r.dirty[k] = &pending{src: src, rec: rec, first: now, last: now}
}

// updateWatches adds every not-yet-watched directory under each detected
// Source's WatchPaths.
func (r *runner) updateWatches() {
	if r.watcher == nil {
		return
	}
	for _, s := range r.detected {
		for _, l := range s.Adapter.Layouts() {
			for _, p := range l.WatchPaths(s.Root) {
				r.watchTree(s.Root, p)
			}
		}
	}
}

// watchTree watches dir and every directory under it. When a watch can't be
// added (say the inotify limit is reached), it logs one warn per root and
// leaves the rest to the rescan.
func (r *runner) watchTree(root, dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || r.watched[p] {
			// A directory that vanished mid-walk is the rescan's to find.
			return nil
		}
		if err := r.watcher.Add(p); err != nil {
			if !r.watchWarned[root] {
				r.Log.Warn("can't watch directory, relying on the rescan for this root",
					"root", root, "dir", p, "watch_limit", watchLimit(), "rescan_interval", r.RescanInterval, "err", err)
				r.watchWarned[root] = true
			}
			return filepath.SkipAll
		}
		r.watched[p] = true
		return nil
	})
}

// watchLimit describes the OS limit a failed watch most likely hit.
func watchLimit() string {
	const inotify = "/proc/sys/fs/inotify/max_user_watches"
	if b, err := os.ReadFile(inotify); err == nil {
		return "fs.inotify.max_user_watches=" + strings.TrimSpace(string(b))
	}
	return "open file limit (kqueue)"
}

func (r *runner) handleEvent(ev fsnotify.Event) {
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		// The OS drops watches on a removed directory and those under it;
		// forget them so a re-created directory is watched again.
		for p := range r.watched {
			if p == ev.Name || strings.HasPrefix(p, ev.Name+string(filepath.Separator)) {
				delete(r.watched, p)
			}
		}
		return
	}
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
		return
	}
	s, ok := r.sourceFor(ev.Name)
	if !ok {
		return
	}
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			r.watchTree(s.Root, ev.Name)
			// Files may have been written before the watch was added.
			filepath.WalkDir(ev.Name, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					r.claim(s, p)
				}
				return nil
			})
			return
		}
	}
	r.claim(s, ev.Name)
}

// claim marks the record at path dirty when one of s's Layouts claims it
// and exclude lets it leave the Machine.
func (r *runner) claim(s Source, path string) {
	for _, l := range s.Adapter.Layouts() {
		if rec, ok := l.Claims(s.Root, path); ok {
			if r.shippable(s, l, rec) {
				r.markDirty(s.Adapter.ID(), rec)
			}
			return
		}
	}
}

// waitingIn yields the waiting records of Source id, keyed as in r.waiting.
func (r *runner) waitingIn(id string) iter.Seq2[string, waiter] {
	return func(yield func(string, waiter) bool) {
		for k, w := range r.waiting {
			if w.src.Adapter.ID() == id && !yield(k, w) {
				return
			}
		}
	}
}

// shippable asks exclude about rec. A record whose Session's cwd can't be
// read yet waits; once rec is decided, records waiting on it as their
// Parent are asked again.
func (r *runner) shippable(s Source, l source.Layout, rec source.Record) bool {
	id := s.Adapter.ID()
	k := cache.Key(id, rec.Key)
	excluded, known := r.Exclude.Excluded(id, l, s.Root, rec)
	if !known {
		r.waiting[k] = waiter{s, l, rec}
		return false
	}
	delete(r.waiting, k)
	for wk, w := range r.waitingIn(id) {
		if p, ok := w.layout.Parent(s.Root, w.rec); !ok || p.Key != rec.Key {
			continue
		}
		if ex, ok := r.Exclude.Excluded(id, w.layout, s.Root, w.rec); ok {
			delete(r.waiting, wk)
			if !ex {
				r.markDirty(id, w.rec)
			}
		}
	}
	return !excluded
}

func (r *runner) sourceFor(path string) (Source, bool) {
	for _, s := range r.detected {
		if path == s.Root || strings.HasPrefix(path, s.Root+string(filepath.Separator)) {
			return s, true
		}
	}
	return Source{}, false
}

func (r *runner) saveCache() {
	r.lastSave = time.Now()
	if err := r.Cache.Save(); err != nil {
		r.Log.Error("writing cache", "err", err)
	}
}

// shutdown stops taking new work, waits up to DrainTimeout for in-flight
// uploads (and a running reconcile's current record), then writes the cache.
func (r *runner) shutdown(cancelUploads context.CancelFunc) error {
	if len(r.inFlight) > 0 || r.reconciling {
		r.Log.Info("shutting down, draining uploads", "in_flight", len(r.inFlight), "reconciling", r.reconciling)
	}
	deadline := time.After(r.DrainTimeout)
	for len(r.inFlight) > 0 || r.reconciling {
		select {
		case res := <-r.results:
			r.handleResult(res)
		case rc := <-r.reconciled:
			r.handleReconciled(rc)
		case <-deadline:
			r.Log.Warn("drain timed out, cancelling uploads", "in_flight", len(r.inFlight))
			cancelUploads()
			deadline = nil
		}
	}
	r.saveCache()
	for _, w := range r.syncQueued {
		w.done <- reconciled{err: reconcile.ErrStopped}
	}
	r.syncQueued = nil
	return nil
}
