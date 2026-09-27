package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/reconcile"
	"github.com/tedkulp/agent-history/internal/collector/status"
	"github.com/tedkulp/agent-history/protocol"
)

// ErrNotRunning is returned by Control calls once Run has returned.
var ErrNotRunning = errors.New("collector not running")

// Control carries requests from other goroutines, such as the control
// socket, into a Run. Its requests run on Run's goroutine, between events.
type Control struct {
	reqs    chan func(*runner)
	stopped chan struct{}
}

// NewControl returns a Control for one Run (Config.Control).
func NewControl() *Control {
	return &Control{reqs: make(chan func(*runner)), stopped: make(chan struct{})}
}

// call runs fn on Run's goroutine.
func (c *Control) call(ctx context.Context, fn func(*runner)) error {
	select {
	case c.reqs <- fn:
		return nil
	case <-c.stopped:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status reports the Hub connection, pending uploads and every enabled
// Source. Fields Run doesn't know, such as the Machine id, are left empty.
func (c *Control) Status(ctx context.Context) (status.Report, error) {
	ch := make(chan status.Report, 1)
	if err := c.call(ctx, func(r *runner) { ch <- r.report() }); err != nil {
		return status.Report{}, err
	}
	return <-ch, nil
}

// Sync runs a full reconcile after any in progress, calling progress with
// lines to show the user, and returns its result. All syncs go through the
// one Run, so two never upload at once.
func (c *Control) Sync(ctx context.Context, progress func(string)) (reconcile.Result, error) {
	w := newSyncWaiter()
	if err := c.call(ctx, func(r *runner) { r.requestSync(w) }); err != nil {
		return reconcile.Result{}, err
	}
	return w.wait(ctx, progress)
}

// SetName changes the display name Run sends to the Hub and sends it now
// with PUT /machines/{id}. The name is kept even when the Hub can't be
// reached; the next reconcile sends it.
func (c *Control) SetName(ctx context.Context, name string) error {
	type put struct {
		hub  reconcile.Hub
		info protocol.MachineInfo
	}
	ch := make(chan put, 1)
	err := c.call(ctx, func(r *runner) {
		r.Info.DisplayName = name
		info := r.Info
		info.Sources = r.sourceInfos()
		ch <- put{r.Hub, info}
	})
	if err != nil {
		return err
	}
	p := <-ch
	return p.hub.PutMachine(ctx, p.info)
}

// Once runs a single full reconcile the way Run would, for `sync` when no
// service is running, and returns its result. The caller holds the lock.
func Once(ctx context.Context, cfg Config, progress func(string)) (reconcile.Result, error) {
	cfg.NoWatch = true
	cfg.Control = nil
	// Waiting from the start means the startup reconcile answers it.
	w := newSyncWaiter()
	cfg.firstSync = w
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	res, err := w.wait(ctx, progress)
	cancel()
	if runErr := <-done; err == nil {
		err = runErr
	}
	return res, err
}

// syncWaiter is one sync waiting for a reconcile to finish.
type syncWaiter struct {
	// progress drops the oldest lines when the waiter is too slow, so
	// the newest line always gets through.
	progress chan string
	done     chan reconciled
}

func newSyncWaiter() *syncWaiter {
	return &syncWaiter{progress: make(chan string, 16), done: make(chan reconciled, 1)}
}

func (w *syncWaiter) say(line string) {
	for {
		select {
		case w.progress <- line:
			return
		default:
		}
		select {
		case <-w.progress:
		default:
		}
	}
}

func (w *syncWaiter) wait(ctx context.Context, progress func(string)) (reconcile.Result, error) {
	for {
		select {
		case line := <-w.progress:
			progress(line)
		case rc := <-w.done:
			for {
				select {
				case line := <-w.progress:
					progress(line)
				default:
					return rc.res, rc.err
				}
			}
		case <-ctx.Done():
			return reconcile.Result{}, ctx.Err()
		}
	}
}

// progressInterval spaces the "N/M records" lines a sync sees.
var progressInterval = time.Second

// progressReporter tells waiters how far a reconcile has got, at most once
// per progressInterval and once at the end.
func progressReporter(waiters []*syncWaiter) func(done, total int) {
	var mu sync.Mutex
	var last time.Time
	return func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now(); done == total || now.Sub(last) >= progressInterval {
			last = now
			for _, w := range waiters {
				w.say(fmt.Sprintf("%d/%d records", done, total))
			}
		}
	}
}

// requestSync queues w for the next reconcile and starts it without
// waiting out any backoff.
func (r *runner) requestSync(w *syncWaiter) {
	if r.reconciling {
		w.say("waiting for the reconcile in progress")
	} else {
		w.say("reconciling")
	}
	r.syncQueued = append(r.syncQueued, w)
	r.needReconcile = true
	r.nextReconcile = time.Time{}
}

// latest is the newer of two failures, either of which may be nil.
func latest(a, b *status.Failure) *status.Failure {
	if a == nil || b != nil && b.At.After(a.At) {
		return b
	}
	return a
}

// sourceInfos is every enabled Source as last discovered.
func (r *runner) sourceInfos() []protocol.SourceInfo {
	infos := []protocol.SourceInfo{}
	for _, s := range r.Sources {
		if si, ok := r.infos[s.Adapter.ID()]; ok {
			infos = append(infos, si)
		}
	}
	return infos
}

func (r *runner) report() status.Report {
	pending := len(r.dirty) + len(r.inFlight)
	rep := status.Report{
		Version:        r.Info.CollectorVersion,
		DisplayName:    r.Info.DisplayName,
		ServiceRunning: true,
		Pending:        &pending,
		Hub:            status.Hub{LastError: r.hubErr},
	}
	if r.contacted {
		online := r.online
		rep.Hub.Reachable = &online
	}
	if r.tooOld != nil {
		rep.Hub.SetUpgradeRequired(r.tooOld.MinVersion)
	}
	if !r.lastSync.IsZero() {
		t := r.lastSync
		rep.Hub.LastSync = &t
	}
	for _, s := range r.Sources {
		id := s.Adapter.ID()
		si := r.infos[id]
		src := status.Source{ID: id, Root: s.Root, Enabled: true, Detected: si.Detected, Layouts: si.Layouts, LastError: latest(r.discoverErr[id], r.uploadErr[id])}
		if si.Detected {
			excluded := r.Exclude.Count(id)
			src.Records, src.Excluded = r.records[id], &excluded
		}
		rep.Sources = append(rep.Sources, src)
	}
	return rep
}
