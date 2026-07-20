package middleware

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a keyed token-bucket limiter with idle eviction. Rules.md §2.13:
// endpoints are limited by default, not as an afterthought.
//
// The map is bounded by eviction rather than by size, which is adequate for a
// single-tenant deployment. A public multi-tenant service would need a
// different structure, and this project is explicitly not that.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	limit    rate.Limit
	burst    int
	idleFor  time.Duration
	nowFn    func() time.Time
	lastGC   time.Time
	gcEveryN time.Duration
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLimiter builds a limiter allowing perMinute events per key per minute,
// with burst capacity equal to perMinute.
func NewLimiter(perMinute int) *Limiter {
	return &Limiter{
		buckets:  make(map[string]*bucket),
		limit:    rate.Every(time.Minute / time.Duration(perMinute)),
		burst:    perMinute,
		idleFor:  10 * time.Minute,
		nowFn:    time.Now,
		gcEveryN: time.Minute,
	}
}

// Allow reports whether one event is permitted for key.
func (l *Limiter) Allow(key string) bool {
	now := l.nowFn()

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > l.gcEveryN {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > l.idleFor {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// RateLimit rejects requests over the per-key budget with a 429. The key is
// the authenticated user when there is one, otherwise the resolved client IP —
// never a client-supplied header.
func RateLimit(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := RealIPFrom(r.Context())
			if uid, ok := UserIDFrom(r.Context()); ok {
				key = "u:" + uid.String()
			}
			if !l.Allow(key) {
				w.Header().Set("Retry-After", "60")
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limited","message":"Too many requests. Slow down."}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ConcurrencyLimiter caps how many requests a single user may have in flight.
// Uploads use it so one client cannot occupy every worker.
type ConcurrencyLimiter struct {
	mu    sync.Mutex
	inUse map[string]int
	max   int
}

// NewConcurrencyLimiter allows max concurrent operations per key.
func NewConcurrencyLimiter(max int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{inUse: make(map[string]int), max: max}
}

// Acquire takes a slot for key. The returned release function is safe to call
// exactly once; it is a no-op when ok is false.
func (c *ConcurrencyLimiter) Acquire(key string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inUse[key] >= c.max {
		return func() {}, false
	}
	c.inUse[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.inUse[key] <= 1 {
				delete(c.inUse, key)
			} else {
				c.inUse[key]--
			}
		})
	}, true
}
