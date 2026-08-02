package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder captures the status code and byte count for the access log
// without buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which is
// what keeps Flush and SetWriteDeadline working on streaming responses.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// levelFor decides how loudly one request is logged.
//
// Not every 5xx is a server fault. 507 means the user's drives are full and
// 503 means a provider is unreachable; both are conditions the user or the
// provider resolves, and logging them at Error fills the error log with
// ordinary traffic until nobody reads it. Error is reserved for the statuses
// that mean this process or something it depends on is genuinely broken.
func levelFor(status int) slog.Level {
	switch {
	case status == http.StatusInternalServerError,
		status == http.StatusBadGateway,
		status == http.StatusGatewayTimeout:
		return slog.LevelError
	case status >= 500:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// Recoverer converts a panic into a 500 rather than a dead process.
// Rules.md §2.6: this is a backstop, not a strategy.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Captured before the handler runs: the deferred closure must not
		// reach back into r, which the handler may have replaced.
		ctx := r.Context()
		reqID := RequestIDFrom(ctx)

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A panic after the client vanished is noise, not a bug, and
			// net/http expects it to keep propagating.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			LoggerFrom(ctx).ErrorContext(ctx, "panic recovered",
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal","message":"Something failed on the server. If this persists, try reconnecting your Google Drive account..","request_id":"` +
				reqID + `"}`))
		}()
		next.ServeHTTP(w, r)
	})
}

// AccessLog logs one line per request. Rules.md §6: method, path, status,
// duration, request id, user id. Never the body, never a query string, since
// share tokens and OAuth codes travel there.
func AccessLog(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := RequestIDFrom(r.Context())
			lg := base.With(slog.String("request_id", reqID))
			ctx := WithLogger(r.Context(), lg)
			r = r.WithContext(ctx)

			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("ip", RealIPFrom(ctx)),
			}
			if uid, ok := UserIDFrom(ctx); ok {
				attrs = append(attrs, slog.String("user_id", uid.String()))
			}
			lg.LogAttrs(ctx, levelFor(rec.status), "request", attrs...)
		})
	}
}
