// Package worker is the Hub's single parse worker: it drains parse_queue,
// parses outside any write transaction, and saves each result in one
// transaction (hub.md §4.5).
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/store"
)

// pollInterval bounds how long the worker sleeps, so it picks up rows written
// by another process.
const pollInterval = 5 * time.Second

// Worker parses queued Sessions.
type Worker struct {
	store   *store.Store
	parsers parser.Registry
	log     *slog.Logger
}

// New returns a worker. A nil logger means slog.Default().
func New(s *store.Store, parsers parser.Registry, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: s, parsers: parsers, log: log}
}

// Run drains the queue until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, nextAt, err := w.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("parse worker", "err", err)
		}
		if worked && err == nil {
			continue
		}
		sleep := pollInterval
		if nextAt > 0 {
			if d := time.Until(time.UnixMilli(nextAt)); d < sleep {
				sleep = max(d, 0)
			}
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-w.store.Wake():
		case <-t.C:
		}
		t.Stop()
	}
}

// RunOnce runs the next due job, if any. It reports whether it ran one, and
// otherwise the earliest not_before in the queue (0 when empty).
func (w *Worker) RunOnce(ctx context.Context) (worked bool, nextAt int64, err error) {
	job, ok, nextAt, err := w.store.NextJob(ctx)
	if err != nil || !ok {
		return false, nextAt, err
	}
	in, ok, err := w.store.LoadParseInput(ctx, job.SessionID)
	if err != nil {
		return false, 0, err
	}
	if !ok {
		// No main record yet: its arrival will enqueue the Session again.
		return true, 0, w.store.DropJob(ctx, job)
	}
	p, ok := w.parsers[in.Source]
	if !ok {
		return true, 0, w.store.DropJob(ctx, job)
	}

	start := time.Now()
	res, perr := safeParse(p, in.Input)
	if perr != nil {
		w.log.Warn("parse failed", "session", job.SessionID, "source", in.Source, "err", perr)
		return true, 0, w.store.SaveFailure(ctx, job, p.Version(), perr.Error())
	}
	if err := w.store.SaveParse(ctx, job, p.Version(), res); err != nil {
		return true, 0, err
	}
	w.log.Debug("parsed session", "session", job.SessionID, "source", in.Source, "messages", len(res.Messages), "duration", time.Since(start))
	return true, 0, nil
}

// safeParse turns a parser panic into an error (hub.md §2.5).
func safeParse(p parser.Parser, in parser.Input) (res parser.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic: %v", r)
		}
	}()
	return p.Parse(in)
}
