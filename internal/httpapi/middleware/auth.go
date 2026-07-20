package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mridul60214/skein/internal/skerr"
)

// Principal is the authenticated caller attached to a request context.
type Principal struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
}

// TokenVerifier validates an access token. It is declared here, in the
// consumer, so the middleware does not import the auth package.
type TokenVerifier interface {
	VerifyAccessToken(token string) (Principal, error)
}

// ErrorWriter renders an error response. httpapi supplies the real one; the
// interface keeps this package free of an import cycle.
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// Auth requires a valid bearer access token. Rules.md §2.15: the token
// arrives in the Authorization header from a JS variable, never from storage,
// so no cookie fallback exists for the access token.
func Auth(v TokenVerifier, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				writeErr(w, r, skerr.Public(skerr.ErrUnauthorized, "Sign in to continue."))
				return
			}
			p, err := v.VerifyAccessToken(raw)
			if err != nil {
				writeErr(w, r, skerr.Public(skerr.ErrUnauthorized, "Your session expired. Sign in again."))
				return
			}
			ctx := WithPrincipal(r.Context(), p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKeyUser, p)
}

// PrincipalFrom returns the authenticated principal in ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKeyUser).(Principal)
	return p, ok
}

// UserIDFrom returns the authenticated user id in ctx.
func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return p.UserID, true
}

// MustUserID returns the authenticated user id. It is only ever called from
// handlers mounted behind Auth, where absence is a wiring bug rather than a
// request condition — hence the sentinel return instead of a panic.
func MustUserID(ctx context.Context) (uuid.UUID, error) {
	id, ok := UserIDFrom(ctx)
	if !ok {
		return uuid.Nil, skerr.ErrUnauthorized
	}
	return id, nil
}
