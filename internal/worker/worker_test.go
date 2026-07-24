package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunnerRunsAtStartAndOnTick(t *testing.T) {
	var runs atomic.Int32
	done := make(chan struct{})

	r := New(discardLogger(), Job{
		Name:       "counter",
		Every:      5 * time.Millisecond,
		RunAtStart: true,
		Run: func(context.Context) error {
			if runs.Add(1) == 3 {
				close(done)
			}
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("job ran %d times, want at least 3", runs.Load())
	}

	cancel()
	r.Wait()
}

func TestRunnerStopsOnContextCancel(t *testing.T) {
	r := New(discardLogger(), Job{
		Name:  "noop",
		Every: time.Millisecond,
		Run:   func(context.Context) error { return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	cancel()

	stopped := make(chan struct{})
	go func() { r.Wait(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after cancellation")
	}
}

// Rules.md §2.6: a panicking job is logged and the loop continues; it must
// never take the process down.
func TestRunnerSurvivesAPanickingJob(t *testing.T) {
	var runs atomic.Int32
	done := make(chan struct{})

	r := New(discardLogger(), Job{
		Name:       "panicker",
		Every:      5 * time.Millisecond,
		RunAtStart: true,
		Run: func(context.Context) error {
			if runs.Add(1) == 3 {
				close(done)
			}
			panic("job blew up")
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("the loop stopped after a panic; ran %d times", runs.Load())
	}

	cancel()
	r.Wait()
}

func TestRunnerContinuesAfterAFailingJob(t *testing.T) {
	var runs atomic.Int32
	done := make(chan struct{})

	r := New(discardLogger(), Job{
		Name:       "failer",
		Every:      5 * time.Millisecond,
		RunAtStart: true,
		Run: func(context.Context) error {
			if runs.Add(1) == 3 {
				close(done)
			}
			return errors.New("provider unreachable")
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("the loop stopped after an error; ran %d times", runs.Load())
	}
	cancel()
	r.Wait()
}

func TestRunnerAppliesAPerRunTimeout(t *testing.T) {
	deadlineSeen := make(chan bool, 1)

	r := New(discardLogger(), Job{
		Name:       "slow",
		Every:      time.Hour,
		RunAtStart: true,
		Timeout:    20 * time.Millisecond,
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			deadlineSeen <- errors.Is(ctx.Err(), context.DeadlineExceeded)
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	select {
	case wasDeadline := <-deadlineSeen:
		if !wasDeadline {
			t.Error("the job was cancelled, but not by its own timeout")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the per-run timeout never fired")
	}
	cancel()
	r.Wait()
}

func TestRunnerSkipsMisconfiguredJobs(t *testing.T) {
	r := New(discardLogger(),
		Job{Name: "no interval", Run: func(context.Context) error { return nil }},
		Job{Name: "no func", Every: time.Millisecond},
	)

	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	cancel()

	stopped := make(chan struct{})
	go func() { r.Wait(); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Wait() blocked on a job that should have been skipped")
	}
}
