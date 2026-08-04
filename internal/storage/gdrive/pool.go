package gdrive

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/mridul249/Skein/internal/storage"
)

// DefaultConcurrency caps how many Drive calls are in flight at once.
//
// Four matches the existing accounts.syncConcurrency, which this pool folds
// in. Rules.md §2.12: bounded parallelism, never one goroutine per row. The
// cap is not primarily about Skein's own resources — it is about not being the
// reason Google starts returning 429 in the first place.
const DefaultConcurrency = 4

// Retry schedule. Deliberately short: these are metadata calls, and a bulk
// delete of fifty files should not take minutes because one shard was
// throttled.
const (
	defaultMaxAttempts = 5
	baseBackoff        = 250 * time.Millisecond
	maxBackoff         = 8 * time.Second
	// maxRetryAfter bounds what a Retry-After header can demand. A provider
	// asking for ten minutes must not pin a worker for ten minutes.
	maxRetryAfter = 30 * time.Second
)

// Pool runs Drive operations with bounded concurrency and retry on rate
// limiting.
//
// One pool serves every bulk call site — bulk delete, empty trash, quota sync,
// and manifest scanning later — so the concurrency cap is global rather than
// per-operation. Two independent bulk operations each politely capped at 4
// still present 8 to Google.
//
// RATE-LIMIT CLASSIFICATION IS NOT REPEATED HERE. apiError already maps 429
// and the 403 rateLimitExceeded / userRateLimitExceeded reasons to
// storage.ErrRateLimited; this pool consumes that sentinel. Re-detecting
// status codes here would be a second classification path that can disagree
// with the first, which is exactly the bug Block 3b fixed (a 403 rate limit
// being read as a revoked grant).
type Pool struct {
	sem         chan struct{}
	maxAttempts int

	// sleep is time.After in production; tests replace it so a retry schedule
	// can be exercised without spending real seconds.
	sleep func(context.Context, time.Duration) error

	mu       sync.Mutex
	inFlight int
	peak     int
}

// PoolOption configures a Pool.
type PoolOption func(*Pool)

// WithConcurrency overrides the default cap. Values below 1 are ignored.
func WithConcurrency(n int) PoolOption {
	return func(p *Pool) {
		if n > 0 {
			p.sem = make(chan struct{}, n)
		}
	}
}

// WithMaxAttempts overrides the retry count. Values below 1 are ignored.
func WithMaxAttempts(n int) PoolOption {
	return func(p *Pool) {
		if n > 0 {
			p.maxAttempts = n
		}
	}
}

// withSleep replaces the backoff sleep. Test support.
func withSleep(f func(context.Context, time.Duration) error) PoolOption {
	return func(p *Pool) { p.sleep = f }
}

// NewPool builds a pool.
func NewPool(opts ...PoolOption) *Pool {
	p := &Pool{
		sem:         make(chan struct{}, DefaultConcurrency),
		maxAttempts: defaultMaxAttempts,
		sleep:       sleepCtx,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do runs fn under the concurrency cap, retrying while it reports rate
// limiting.
//
// Only storage.ErrRateLimited is retried. Every other failure — a revoked
// grant, a missing object, a size mismatch — is returned immediately, because
// retrying it would turn one clear error into the same error five times
// slower.
func (p *Pool) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()

	p.enter()
	defer p.leave()

	var lastErr error
	for attempt := 0; attempt < p.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrRateLimited) {
			return err
		}
		lastErr = err

		// Last attempt: do not sleep before giving up.
		if attempt == p.maxAttempts-1 {
			break
		}
		if serr := p.sleep(ctx, backoffFor(attempt, err)); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", p.maxAttempts, lastErr)
}

// enter/leave track observed concurrency so a test can assert the cap actually
// held, rather than trusting that the semaphore is wired correctly.
func (p *Pool) enter() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
}

func (p *Pool) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight--
}

// PeakConcurrency reports the most operations ever in flight at once.
func (p *Pool) PeakConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// backoffFor returns how long to wait before the next attempt.
//
// A provider-supplied Retry-After wins when present: it is the provider saying
// how long it actually needs, and guessing shorter just earns another 429.
// Otherwise exponential with FULL JITTER — random in [0, cap) rather than
// cap±small. Without jitter, fifty shards throttled by one burst all retry at
// the same instant and reproduce the burst that caused the throttle.
func backoffFor(attempt int, err error) time.Duration {
	if d, ok := RetryAfterFrom(err); ok {
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		if d > 0 {
			return d
		}
	}

	backoff := baseBackoff << attempt
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	return time.Duration(rand.Int64N(int64(backoff)) + int64(baseBackoff)/2)
}

// RateLimitError carries a provider Retry-After alongside the sentinel.
type RateLimitError struct {
	RetryAfter time.Duration
	cause      error
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited, retry after %s: %v", e.RetryAfter, e.cause)
	}
	return fmt.Sprintf("rate limited: %v", e.cause)
}

// Unwrap exposes both the sentinel and the underlying cause, so
// errors.Is(err, storage.ErrRateLimited) keeps working through it.
func (e *RateLimitError) Unwrap() []error {
	return []error{storage.ErrRateLimited, e.cause}
}

// RetryAfterFrom extracts a provider-supplied delay, if there is one.
func RetryAfterFrom(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return rl.RetryAfter, true
	}
	return 0, false
}

// parseRetryAfter reads the header in both forms RFC 9110 allows: delay in
// seconds, or an HTTP date.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
		return secs
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
