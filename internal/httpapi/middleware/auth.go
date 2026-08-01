package middleware

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/skerr"
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

// CapabilityVerifier validates a content grant carried in the query string.
// Declared here, in the consumer, so the middleware does not import the
// capability package.
type CapabilityVerifier interface {
	// Verify returns the user a grant authorises for fileID, or an error.
	// The error is deliberately undifferentiated; callers must not try to
	// tell expired from forged.
	Verify(fileID uuid.UUID, q url.Values, now time.Time) (uuid.UUID, error)
}

// ContentAuth accepts either a bearer access token or a signed capability URL.
//
// The second credential exists because a browser-managed transfer cannot set
// an Authorization header, and routing a download through fetch() instead means
// buffering the response in JS — known issue #15. Uploads keep plain Auth: only
// reads are reachable this way, and only for one file.
//
// fileID extracts the requested file from the route, so the signature is
// checked against the file actually being served rather than one named in the
// query. It is a parameter rather than a chi call so this package stays free of
// the router.
func ContentAuth(
	v TokenVerifier,
	caps CapabilityVerifier,
	fileID func(*http.Request) (uuid.UUID, bool),
	writeErr ErrorWriter,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A session always wins. The capability path is only for
			// requests the browser issues on its own, which never carry
			// the header.
			if raw := bearerToken(r); raw != "" {
				p, err := v.VerifyAccessToken(raw)
				if err != nil {
					writeErr(w, r, skerr.Public(skerr.ErrUnauthorized, "Your session expired. Sign in again."))
					return
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}

			if caps == nil {
				writeErr(w, r, skerr.Public(skerr.ErrUnauthorized, "Sign in to continue."))
				return
			}
			id, ok := fileID(r)
			if !ok {
				writeErr(w, r, errBadGrant)
				return
			}
			// Verified once, here, at the start of the request. The
			// stream this authorises may then run for hours; see
			// capability.TTL for why that is intended.
			uid, err := caps.Verify(id, r.URL.Query(), time.Now())
			if err != nil {
				writeErr(w, r, errBadGrant)
				return
			}
			// SessionID stays zero: a grant is not a session, and nothing
			// on the content path reads it. Anything that later needs a
			// session id must not be mounted behind this middleware.
			p := Principal{UserID: uid}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// errBadGrant is the single answer to every bad capability: forged, tampered,
// malformed and expired are indistinguishable to the caller by design.
var errBadGrant = skerr.Public(skerr.ErrUnauthorized, "That download link is no longer valid.")

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
