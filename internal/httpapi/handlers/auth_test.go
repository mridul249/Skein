package handlers_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/httpapi/handlers"
	"github.com/mridul60214/skein/internal/httpapi/httpx"
	"github.com/mridul60214/skein/internal/httpapi/middleware"
)

// The handler tests exercise the real service against the package's in-memory
// store via the exported constructor, so the wire contract — status codes,
// cookie attributes, and what does and does not appear in a response — is
// covered without a database.

const (
	testEmail    = "mridul@example.com"
	testPassword = "correct horse battery staple"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()

	svc := auth.NewService(
		auth.NewMemoryStore(),
		auth.NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute),
		720*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	h := handlers.NewAuth(svc, true)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(nil))
	r.Use(middleware.Recoverer)
	r.Use(middleware.MaxJSONBody(1 << 20))
	r.Route("/api/auth", func(a chi.Router) {
		a.Post("/register", h.Register)
		a.Post("/login", h.Login)
		a.Post("/refresh", h.Refresh)
		a.Post("/logout", h.Logout)
		a.With(middleware.Auth(svc, httpx.WriteError)).Get("/me", h.Me)
	})
	return r
}

func postJSON(t *testing.T, h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.7:4000"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func credentials(email, password string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return string(b)
}

func decodeSession(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

func refreshCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == handlers.RefreshCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response", handlers.RefreshCookieName)
	return nil
}

func TestRegisterReturns201AndSetsCookie(t *testing.T) {
	r := newTestRouter(t)

	rec := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	body := decodeSession(t, rec)
	if body["access_token"] == "" || body["access_token"] == nil {
		t.Error("no access token in the response")
	}

	// Rules.md §2.15: the refresh token lives only in an httpOnly cookie,
	// and must never appear in the JSON the page can read.
	if strings.Contains(rec.Body.String(), "refresh") {
		t.Errorf("the response body mentions the refresh token: %s", rec.Body)
	}

	c := refreshCookie(t, rec)
	if !c.HttpOnly {
		t.Error("refresh cookie is not HttpOnly")
	}
	if !c.Secure {
		t.Error("refresh cookie is not Secure")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}
	if c.Path != "/api/auth" {
		t.Errorf("cookie path = %q, want /api/auth", c.Path)
	}
	if c.Value == "" {
		t.Error("refresh cookie is empty")
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	r := newTestRouter(t)

	if rec := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword)); rec.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", rec.Code)
	}
	rec := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
}

func TestRegisterValidationReturns400WithFields(t *testing.T) {
	r := newTestRouter(t)

	rec := postJSON(t, r, "/api/auth/register", credentials("nope", "short"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error != "validation_failed" {
		t.Errorf("error = %q, want validation_failed", body.Error)
	}
	if _, ok := body.Fields["email"]; !ok {
		t.Errorf("no email field error: %+v", body.Fields)
	}
	if body.RequestID == "" {
		t.Error("no request id in the error body")
	}
}

func TestLoginWrongPasswordReturns401(t *testing.T) {
	r := newTestRouter(t)
	postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))

	rec := postJSON(t, r, "/api/auth/login", credentials(testEmail, "wrong password entirely"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body)
	}
	// No stack, no SQL, no internal text.
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), "sql") {
		t.Errorf("internal detail leaked: %s", rec.Body)
	}
}

func TestMeRequiresAValidAccessToken(t *testing.T) {
	r := newTestRouter(t)
	reg := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	token, _ := decodeSession(t, reg)["access_token"].(string)

	tests := []struct {
		name       string
		authHeader string
		want       int
	}{
		{"valid bearer", "Bearer " + token, http.StatusOK},
		{"lowercase scheme", "bearer " + token, http.StatusOK},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"garbage token", "Bearer nonsense", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestRefreshRotatesTheCookie(t *testing.T) {
	r := newTestRouter(t)
	reg := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	first := refreshCookie(t, reg)

	rec := postJSON(t, r, "/api/auth/refresh", "", first)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	second := refreshCookie(t, rec)
	if second.Value == first.Value {
		t.Fatal("the refresh cookie was not rotated")
	}
}

// The Phase 1 exit criterion, seen from the wire: replaying a spent cookie
// fails and clears the cookie so the browser stops retrying a dead family.
func TestRefreshReuseIsRejectedAndClearsTheCookie(t *testing.T) {
	r := newTestRouter(t)
	reg := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	first := refreshCookie(t, reg)

	rotated := postJSON(t, r, "/api/auth/refresh", "", first)
	if rotated.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d", rotated.Code)
	}
	second := refreshCookie(t, rotated)

	replay := postJSON(t, r, "/api/auth/refresh", "", first)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401: %s", replay.Code, replay.Body)
	}
	if cleared := refreshCookie(t, replay); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("the cookie was not cleared on replay: %+v", cleared)
	}

	// The legitimate successor is dead too — the whole family went.
	after := postJSON(t, r, "/api/auth/refresh", "", second)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("post-reuse refresh status = %d, want 401", after.Code)
	}
}

func TestRefreshWithNoCookieReturns401(t *testing.T) {
	r := newTestRouter(t)
	rec := postJSON(t, r, "/api/auth/refresh", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestLogoutClearsTheCookieAndIsIdempotent(t *testing.T) {
	r := newTestRouter(t)
	reg := postJSON(t, r, "/api/auth/register", credentials(testEmail, testPassword))
	c := refreshCookie(t, reg)

	rec := postJSON(t, r, "/api/auth/logout", "", c)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if cleared := refreshCookie(t, rec); cleared.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative", cleared.MaxAge)
	}

	if again := postJSON(t, r, "/api/auth/logout", "", c); again.Code != http.StatusNoContent {
		t.Errorf("second logout status = %d, want 204", again.Code)
	}
	if none := postJSON(t, r, "/api/auth/logout", ""); none.Code != http.StatusNoContent {
		t.Errorf("logout with no cookie status = %d, want 204", none.Code)
	}
}

// Rules.md §2.6: malformed input is a 400, never a panic and never a 500.
func TestMalformedBodiesAreRejectedCleanly(t *testing.T) {
	r := newTestRouter(t)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty", "", http.StatusBadRequest},
		{"not json", "<html>", http.StatusBadRequest},
		{"truncated", `{"email":`, http.StatusBadRequest},
		{"wrong types", `{"email":123,"password":true}`, http.StatusBadRequest},
		{"unknown field", `{"email":"a@b.co","password":"x","admin":true}`, http.StatusBadRequest},
		{"two objects", `{"email":"a@b.co","password":"correct horse battery"}{"x":1}`, http.StatusBadRequest},
		{"null", `null`, http.StatusBadRequest},
		{"array", `[]`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, r, "/api/auth/register", tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestNonJSONContentTypeIsRejected(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register",
		strings.NewReader(credentials(testEmail, testPassword)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestOversizedBodyReturns413(t *testing.T) {
	r := newTestRouter(t)
	huge := `{"email":"a@b.co","password":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := postJSON(t, r, "/api/auth/register", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413: %s", rec.Code, rec.Body)
	}
}
