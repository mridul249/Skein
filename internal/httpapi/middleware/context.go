// Package middleware holds the HTTP middleware chain. Order is defined once,
// in httpapi.Server; see Architecture.md §9.
package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
	ctxKeyRealIP
	ctxKeyUser
)

// RequestID assigns every request an id, echoes it in a header, and puts it in
// the context so error responses and log lines can be correlated.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom returns the request id in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// WithLogger stores lg in ctx.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, lg)
}

// LoggerFrom returns the request-scoped logger, or the default one.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if lg, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && lg != nil {
		return lg
	}
	return slog.Default()
}

// RealIPFrom returns the client IP decided by the RealIP middleware. It falls
// back to RemoteAddr, never to an unverified header.
func RealIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(ctxKeyRealIP).(string)
	return ip
}
