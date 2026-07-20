package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Rules.md §2.6: a malformed field must never take down the process.
func TestRecovererTurnsPanicInto500(t *testing.T) {
	h := RequestID(AccessLog(discardLogger())(Recoverer(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}))))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "boom") {
		t.Errorf("panic value leaked to the client: %s", body)
	}
	if !strings.Contains(body, `"error":"internal"`) {
		t.Errorf("body = %s, want the typed error shape", body)
	}
}

func TestRequestIDIsSetAndEchoed(t *testing.T) {
	var inCtx string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inCtx = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if inCtx == "" {
		t.Fatal("no request id in context")
	}
	if got := rec.Header().Get("X-Request-Id"); got != inCtx {
		t.Errorf("header = %q, context = %q; want equal", got, inCtx)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestCORSAllowlist(t *testing.T) {
	h := CORS([]string{"https://app.example"})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	t.Run("allowed origin echoes back", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://app.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("allow-origin = %q", got)
		}
	})

	t.Run("unknown origin gets no allow header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("allow-origin = %q, want empty", got)
		}
	})

	t.Run("unknown origin preflight is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})
}

// Rules.md §2.13 and the Phase 1 exit criterion: the 6th auth attempt in a
// minute is a 429.
func TestRateLimitReturns429OnSixthAttempt(t *testing.T) {
	lim := NewLimiter(5)
	h := RealIP(nil)(RateLimit(lim)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))

	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "198.51.100.10:5000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 1; i <= 5; i++ {
		if got := call(); got != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i, got)
		}
	}
	if got := call(); got != http.StatusTooManyRequests {
		t.Fatalf("attempt 6: status = %d, want 429", got)
	}
}

func TestRateLimitIsPerKey(t *testing.T) {
	lim := NewLimiter(2)
	for i := 1; i <= 2; i++ {
		if !lim.Allow("a") {
			t.Fatalf("call %d for key a should pass", i)
		}
	}
	if lim.Allow("a") {
		t.Fatal("third for key a should be denied")
	}
	if !lim.Allow("b") {
		t.Fatal("key b has its own budget")
	}
}

func TestLimiterEvictsIdleBuckets(t *testing.T) {
	now := time.Now()
	lim := NewLimiter(5)
	lim.nowFn = func() time.Time { return now }

	lim.Allow("stale")
	now = now.Add(30 * time.Minute)
	lim.Allow("fresh")

	lim.mu.Lock()
	_, stillThere := lim.buckets["stale"]
	lim.mu.Unlock()
	if stillThere {
		t.Error("idle bucket was not evicted")
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	cl := NewConcurrencyLimiter(2)

	r1, ok := cl.Acquire("u1")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	r2, ok := cl.Acquire("u1")
	if !ok {
		t.Fatal("second acquire should succeed")
	}
	if _, ok := cl.Acquire("u1"); ok {
		t.Fatal("third acquire should be refused")
	}
	if _, ok := cl.Acquire("u2"); !ok {
		t.Fatal("a different user has its own budget")
	}

	r1()
	r1() // release is idempotent
	if _, ok := cl.Acquire("u1"); !ok {
		t.Fatal("slot should be free after release")
	}
	r2()
}

func TestConcurrencyLimiterIsRaceFree(t *testing.T) {
	cl := NewConcurrencyLimiter(4)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := cl.Acquire("shared"); ok {
				release()
			}
		}()
	}
	wg.Wait()

	if release, ok := cl.Acquire("shared"); !ok {
		t.Fatal("all slots should be released")
	} else {
		release()
	}
}

func TestMaxJSONBodyCaps(t *testing.T) {
	h := MaxJSONBody(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}
