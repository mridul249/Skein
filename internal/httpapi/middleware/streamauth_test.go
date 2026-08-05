package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// EventSource CANNOT SET HEADERS. There is no option for it in the API, so the
// SSE progress stream is unreachable behind header-only Auth: the browser
// opens the connection with no Authorization, gets a 401, and EventSource
// retries that failure forever. The desktop download drawer therefore renders
// with a permanently frozen 0-byte row.
//
// Verified against the RUNNING desktop app before this test was written:
//
//	GET /api/desktop/downloads/{id}/events        -> 401
//	GET  ... with "Authorization: Bearer <token>"  -> 404 (reached the handler)
//
// So StreamAuth accepts the same access token in the query string too. It is
// GET-only and read-only by construction: the routes it guards stream state,
// and a credential that can reach them can reach nothing that writes.
func TestStreamAuthAcceptsATokenTheEventSourceCanCarry(t *testing.T) {
	t.Parallel()

	user := uuid.New()
	h := middleware.StreamAuth(stubVerifier{user: user}, httpx.WriteError)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := middleware.MustUserID(r.Context())
			if err != nil {
				t.Errorf("handler ran without a principal: %v", err)
			}
			if got != user {
				t.Errorf("principal = %s, want %s", got, user)
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	cases := []struct {
		name string
		url  string
		hdr  string
		want int
	}{
		{
			name: "a query token is accepted, because EventSource has no other way",
			url:  "/events?access_token=valid",
			want: http.StatusOK,
		},
		{name: "the header still works", url: "/events", hdr: "Bearer valid", want: http.StatusOK},
		{name: "a forged query token is refused", url: "/events?access_token=forged", want: http.StatusUnauthorized},
		{name: "no credential at all is refused", url: "/events", want: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.hdr != "" {
				req.Header.Set("Authorization", tc.hdr)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// A token in a query string is more exposed than one in a header, so the same
// scrubbing requirement TestAccessLogNeverCarriesTheSignature imposes on the
// capability signature applies here: the log records the path, never the query.
func TestStreamAuthTokenNeverReachesTheAccessLog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := middleware.RequestID(middleware.AccessLog(lg)(
		middleware.StreamAuth(stubVerifier{user: uuid.New()}, httpx.WriteError)(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	))

	req := httptest.NewRequest(http.MethodGet, "/events?access_token=valid", nil)
	req.RemoteAddr = "198.51.100.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("nothing was logged; this test would pass vacuously")
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, logged)
	}
	if line["path"] != "/events" {
		t.Fatalf("logged path = %v, want /events", line["path"])
	}
	if strings.Contains(logged, "access_token") || strings.Contains(logged, "valid") {
		t.Fatalf("the access token reached the access log:\n%s", logged)
	}
}
