package middleware

import (
	"net/http"

	"github.com/mridul249/Skein/internal/skerr"
)

// StreamAuth requires a valid access token, carried EITHER in the
// Authorization header or in the `access_token` query parameter.
//
// The query form exists for exactly one reason: EventSource cannot set
// headers. There is no option for it in the API, so an SSE route behind
// header-only Auth is unreachable from a browser — it answers 401, and
// EventSource retries that failure forever rather than surfacing it. That is
// how the desktop download drawer shipped with a permanently frozen 0-byte
// row while every piece behind it worked.
//
// The exposure a query credential adds is bounded deliberately:
//
//   - GET only. The routes this guards stream state and write nothing, so a
//     credential that reaches them cannot mutate anything.
//   - The access log records the path and never the query
//     (TestStreamAuthTokenNeverReachesTheAccessLog), the same property the
//     capability signature already relies on.
//   - It is the ordinary access token, which already lives for 15 minutes and
//     is already the credential every other authenticated route accepts. This
//     adds a second CARRIER, not a second credential — unlike a capability
//     grant, nothing new becomes forgeable or outlives what already existed.
func StreamAuth(v TokenVerifier, writeErr ErrorWriter) func(http.Handler) http.Handler {
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
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
