package gdrive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mridul249/Skein/internal/storage"
)

// noSleep makes the retry schedule instant so tests exercise the LOGIC without
// spending real seconds. The delays themselves are asserted separately in
// TestBackoffHonoursRetryAfter.
func noSleep() PoolOption {
	return withSleep(func(context.Context, time.Duration) error { return nil })
}

// MUTATION TARGET 1. A backend that rate-limits the first N calls must
// COMPLETE via retry, not fail.
func TestPoolRetriesThroughRateLimiting(t *testing.T) {
	for _, failures := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("%d_429s", failures), func(t *testing.T) {
			var calls atomic.Int32
			p := NewPool(noSleep(), WithMaxAttempts(5))

			err := p.Do(context.Background(), func(context.Context) error {
				if int(calls.Add(1)) <= failures {
					return &RateLimitError{cause: errors.New("slow down")}
				}
				return nil
			})

			if err != nil {
				t.Fatalf("Do() = %v after %d rate limits; it must retry through them",
					err, failures)
			}
			if got := int(calls.Load()); got != failures+1 {
				t.Errorf("%d calls, want %d (one per failure plus the success)",
					got, failures+1)
			}
		})
	}
}

// Retrying is bounded. A permanently throttled backend must eventually give up
// rather than spin forever, and the final error must still say it was rate
// limiting.
func TestPoolGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	p := NewPool(noSleep(), WithMaxAttempts(3))

	err := p.Do(context.Background(), func(context.Context) error {
		calls.Add(1)
		return &RateLimitError{cause: errors.New("still limited")}
	})

	if err == nil {
		t.Fatal("Do() = nil against a permanently rate-limited backend")
	}
	if !errors.Is(err, storage.ErrRateLimited) {
		t.Errorf("error = %v, want it to still match ErrRateLimited", err)
	}
	if got := int(calls.Load()); got != 3 {
		t.Errorf("%d attempts, want exactly 3", got)
	}
}

// ONLY rate limiting is retried. Retrying a revoked grant or a missing object
// turns one clear error into the same error five times slower — and, worse,
// five times the load on a provider that already said no.
func TestPoolDoesNotRetryNonRateLimitErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"revoked grant", storage.ErrUnauthorized},
		{"object missing", storage.ErrObjectNotFound},
		{"out of space", storage.ErrQuota},
		{"size mismatch", storage.ErrSizeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := NewPool(noSleep(), WithMaxAttempts(5))

			err := p.Do(context.Background(), func(context.Context) error {
				calls.Add(1)
				return tc.err
			})

			if !errors.Is(err, tc.err) {
				t.Errorf("error = %v, want %v unchanged", err, tc.err)
			}
			if got := int(calls.Load()); got != 1 {
				t.Errorf("%d calls for a non-retryable error, want 1", got)
			}
		})
	}
}

// MUTATION TARGET 2. An unbounded burst must not exceed the cap.
//
// Concurrency is OBSERVED — every worker records its own arrival and departure
// — rather than inferred from the semaphore being present. A semaphore that is
// created but never acquired would pass a structural check and fail this one.
func TestPoolEnforcesTheConcurrencyCap(t *testing.T) {
	const (
		cap    = 3
		bursts = 60
	)
	p := NewPool(WithConcurrency(cap), noSleep())

	var (
		mu       sync.Mutex
		inFlight int
		observed int
	)

	var wg sync.WaitGroup
	for i := 0; i < bursts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Do(context.Background(), func(context.Context) error {
				mu.Lock()
				inFlight++
				if inFlight > observed {
					observed = inFlight
				}
				mu.Unlock()

				// Hold the slot long enough that an uncapped implementation
				// would pile up visibly.
				time.Sleep(2 * time.Millisecond)

				mu.Lock()
				inFlight--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if observed > cap {
		t.Errorf("observed %d concurrent operations with a cap of %d; "+
			"the burst was not bounded", observed, cap)
	}
	if observed < 2 {
		t.Errorf("observed peak concurrency %d; the test did not actually "+
			"exercise parallelism", observed)
	}
	if got := p.PeakConcurrency(); got > cap {
		t.Errorf("pool reports peak %d, above the cap %d", got, cap)
	}
}

// A provider-supplied Retry-After wins over the computed backoff: it is the
// provider saying how long it actually needs, and guessing shorter earns
// another 429.
func TestBackoffHonoursRetryAfter(t *testing.T) {
	err := &RateLimitError{RetryAfter: 3 * time.Second, cause: errors.New("x")}
	if got := backoffFor(0, err); got != 3*time.Second {
		t.Errorf("backoff = %v, want the provider's 3s", got)
	}

	// Bounded: a provider asking for ten minutes must not pin a worker.
	long := &RateLimitError{RetryAfter: 10 * time.Minute, cause: errors.New("x")}
	if got := backoffFor(0, long); got > maxRetryAfter {
		t.Errorf("backoff = %v, want it capped at %v", got, maxRetryAfter)
	}
}

// Without a Retry-After the delay grows and is jittered. Full jitter matters:
// fifty shards throttled by one burst must not all retry at the same instant
// and reproduce the burst.
func TestBackoffIsExponentialAndJittered(t *testing.T) {
	plain := &RateLimitError{cause: errors.New("x")}

	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[backoffFor(3, plain)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays across 50 draws; the backoff is not jittered",
			len(seen))
	}

	// And it grows with the attempt number.
	var early, late time.Duration
	for i := 0; i < 200; i++ {
		if d := backoffFor(0, plain); d > early {
			early = d
		}
		if d := backoffFor(4, plain); d > late {
			late = d
		}
	}
	if late <= early {
		t.Errorf("max delay at attempt 4 (%v) is not above attempt 0 (%v); "+
			"the backoff is not exponential", late, early)
	}
	if late > maxBackoff+baseBackoff {
		t.Errorf("delay %v exceeds the ceiling %v", late, maxBackoff)
	}
}

// Cancellation beats retrying: a cancelled bulk operation must stop, not
// finish its retry schedule.
func TestPoolStopsRetryingOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32

	p := NewPool(WithMaxAttempts(10), withSleep(func(c context.Context, _ time.Duration) error {
		return c.Err()
	}))

	err := p.Do(ctx, func(context.Context) error {
		if calls.Add(1) == 2 {
			cancel()
		}
		return &RateLimitError{cause: errors.New("limited")}
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got := int(calls.Load()); got > 3 {
		t.Errorf("%d calls after cancellation; it kept retrying", got)
	}
}

// The pool consumes the sentinel apiError already produces. This pins the
// single-classification-path property: if apiError stopped returning
// ErrRateLimited for a 429, the pool would silently stop retrying.
func TestPoolRetriesWhatApiErrorClassifies(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"2"}},
		Body:       http.NoBody,
	}
	b := &Backend{}
	err := b.apiError(resp, "delete object")

	if !errors.Is(err, storage.ErrRateLimited) {
		t.Fatalf("apiError(429) = %v, want it to match ErrRateLimited; "+
			"the pool keys its retry decision on that sentinel", err)
	}
	if d, ok := RetryAfterFrom(err); !ok || d != 2*time.Second {
		t.Errorf("RetryAfterFrom = %v, %v; want the provider's 2s", d, ok)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		hdr  string
		want time.Duration
	}{
		{"seconds", "5", 5 * time.Second},
		{"zero", "0", 0},
		{"absent", "", 0},
		{"http date", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"date in the past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon please", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.hdr != "" {
				h.Set("Retry-After", tc.hdr)
			}
			if got := parseRetryAfter(h, now); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.hdr, got, tc.want)
			}
		})
	}
}
