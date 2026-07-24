// Package worker runs the background loops: quota refresh, reservation
// reclaim, and expired-row cleanup.
//
// Architecture.md §11: every goroutine takes a context and has a defined exit,
// and none of these ever runs on the upload hot path.
package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Job is one unit of periodic work.
type Job struct {
	Name string
	// Every is the interval between runs.
	Every time.Duration
	// RunAtStart makes the job fire once immediately, so a freshly booted
	// process does not show stale numbers for a full interval.
	RunAtStart bool
	// Run does the work. It returns an error for logging only; a failing
	// job is retried on the next tick rather than abandoned.
	Run func(ctx context.Context) error
	// Timeout bounds a single run so a hung provider cannot wedge the loop.
	Timeout time.Duration
}

// Runner supervises a set of periodic jobs.
type Runner struct {
	log  *slog.Logger
	jobs []Job
	wg   sync.WaitGroup
}

// New builds a runner.
func New(log *slog.Logger, jobs ...Job) *Runner {
	return &Runner{log: log, jobs: jobs}
}

// Start launches every job. It returns immediately; Wait blocks until all of
// them have stopped after ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	for _, j := range r.jobs {
		if j.Every <= 0 || j.Run == nil {
			r.log.Warn("skipping misconfigured job", slog.String("job", j.Name))
			continue
		}
		r.wg.Add(1)
		go r.loop(ctx, j)
	}
}

// Wait blocks until every job has exited.
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) loop(ctx context.Context, j Job) {
	defer r.wg.Done()

	ticker := time.NewTicker(j.Every)
	defer ticker.Stop()

	if j.RunAtStart {
		r.runOnce(ctx, j)
	}

	for {
		select {
		case <-ctx.Done():
			r.log.Debug("job stopped", slog.String("job", j.Name))
			return
		case <-ticker.C:
			r.runOnce(ctx, j)
		}
	}
}

// runOnce executes a job with its own recover, so a panic in one loop cannot
// take the process down. Rules.md §2.6: every spawned goroutine has one.
func (r *Runner) runOnce(ctx context.Context, j Job) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("job panicked",
				slog.String("job", j.Name),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
		}
	}()

	runCtx := ctx
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, j.Timeout)
		defer cancel()
	}

	start := time.Now()
	if err := j.Run(runCtx); err != nil {
		// A cancelled context during shutdown is expected, not a failure.
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("job failed",
			slog.String("job", j.Name),
			slog.Duration("duration", time.Since(start)),
			slog.String("error", err.Error()))
		return
	}
	r.log.Debug("job finished",
		slog.String("job", j.Name),
		slog.Duration("duration", time.Since(start)))
}
