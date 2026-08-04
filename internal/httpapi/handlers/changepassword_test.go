package handlers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mridul249/Skein/internal/auth"
	"github.com/mridul249/Skein/internal/httpapi/handlers"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// authRatePerMin mirrors httpapi's own constant. Duplicated rather than
// exported: the point of these tests is that change-password sits on the
// credential budget, so the number they assert against should fail loudly if
// somebody changes one and not the other.
const authRatePerMin = 5

// newCredentialRouter mirrors the real wiring in server.go: register, login and
// change-password share ONE credential limiter, and change-password also
// requires an authenticated session.
func newCredentialRouter(t *testing.T) http.Handler {
	t.Helper()

	svc := auth.NewService(
		auth.NewMemoryStore(),
		auth.NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute),
		720*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	h := handlers.NewAuth(svc, true)
	limiter := middleware.NewLimiter(authRatePerMin)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP(nil))
	r.Use(middleware.MaxJSONBody(1 << 20))
	r.Route("/api/auth", func(a chi.Router) {
		a.Group(func(pub chi.Router) {
			pub.Use(middleware.RateLimit(limiter))
			pub.Post("/register", h.Register)
			pub.Post("/login", h.Login)
		})
		a.Group(func(cred chi.Router) {
			cred.Use(middleware.Auth(svc, httpx.WriteError))
			cred.Use(middleware.RateLimit(limiter))
			cred.Post("/change-password", h.ChangePassword)
		})
	})
	return r
}

// registerAndToken creates a user and returns its bearer token.
func registerAndToken(t *testing.T, h http.Handler, email, password string) string {
	t.Helper()
	rec := postJSON(t, h, "/api/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d, body %s", rec.Code, rec.Body.String())
	}
	tok, _ := decodeSession(t, rec)["access_token"].(string)
	if tok == "" {
		t.Fatal("register returned no access token")
	}
	return tok
}

func changePassword(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	cpEmail = "changer@example.com"
	cpOld   = "correct horse battery staple"
	cpNew   = "an entirely different long password"
)

// Unauthenticated callers get 401. The endpoint is not a password-reset flow.
func TestChangePasswordRequiresASession(t *testing.T) {
	r := newCredentialRouter(t)
	rec := changePassword(t, r, "",
		`{"current_password":"`+cpOld+`","new_password":"`+cpNew+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestChangePasswordSucceedsAndOldPasswordStopsWorking(t *testing.T) {
	r := newCredentialRouter(t)
	tok := registerAndToken(t, r, cpEmail, cpOld)

	rec := changePassword(t, r, tok,
		`{"current_password":"`+cpOld+`","new_password":"`+cpNew+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("204 carried a body: %q", body)
	}

	if got := postJSON(t, r, "/api/auth/login",
		`{"email":"`+cpEmail+`","password":"`+cpNew+`"}`); got.Code != http.StatusOK {
		t.Errorf("login with the new password = %d, want 200", got.Code)
	}
	if got := postJSON(t, r, "/api/auth/login",
		`{"email":"`+cpEmail+`","password":"`+cpOld+`"}`); got.Code == http.StatusOK {
		t.Error("the old password still logs in")
	}
}

func TestChangePasswordRejectsAWrongCurrentPassword(t *testing.T) {
	r := newCredentialRouter(t)
	tok := registerAndToken(t, r, cpEmail, cpOld)

	rec := changePassword(t, r, tok,
		`{"current_password":"wrong password entirely","new_password":"`+cpNew+`"}`)

	// NOT 401. The frontend clears the Skein session on any 401 and retries
	// once after a refresh, so returning 401 for a typo signs the user out
	// before the error can render — and burns two of the 5/min budget.
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("a wrong current password returned 401; the user is signed out " +
			"instead of seeing the error")
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want a validation status", rec.Code)
	}
	// The field is named so the modal can render it inline.
	if !strings.Contains(rec.Body.String(), "current_password") {
		t.Errorf("body does not name the field: %s", rec.Body.String())
	}

	// THE SESSION SURVIVES. A second wrong attempt with the same token must
	// still be authenticated — it fails validation again, not authentication.
	// Deliberately another WRONG password so the check does not mutate state.
	after := changePassword(t, r, tok,
		`{"current_password":"still not right","new_password":"`+cpNew+`"}`)
	if after.Code == http.StatusUnauthorized {
		t.Error("the session was destroyed by a failed change-password attempt")
	}
	// And nothing changed.
	if got := postJSON(t, r, "/api/auth/login",
		`{"email":"`+cpEmail+`","password":"`+cpOld+`"}`); got.Code != http.StatusOK {
		t.Error("the original password stopped working after a rejected change")
	}
}

func TestChangePasswordRejectsAWeakNewPassword(t *testing.T) {
	r := newCredentialRouter(t)
	tok := registerAndToken(t, r, cpEmail, cpOld)

	rec := changePassword(t, r, tok,
		`{"current_password":"`+cpOld+`","new_password":"short"}`)
	if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want a validation failure", rec.Code)
	}
}

// THE RATE LIMITER IS THE POINT OF THIS ROUTE'S PLACEMENT. Change-password
// verifies a password, so it is an online oracle; it must sit on the 5/min
// credential budget, not the 300/min API one. The 6th attempt in a minute is
// refused.
//
// Wrong current passwords are used deliberately: a limiter that only engaged
// on success would be useless against guessing.
func TestChangePasswordIsOnTheCredentialRateLimit(t *testing.T) {
	r := newCredentialRouter(t)
	tok := registerAndToken(t, r, cpEmail, cpOld)

	// Registration already spent one unit of the shared budget.
	var lastCode int
	limited := false
	for i := 0; i < authRatePerMin+2; i++ {
		rec := changePassword(t, r, tok,
			`{"current_password":"guess number `+string(rune('a'+i))+` wrong","new_password":"`+cpNew+`"}`)
		lastCode = rec.Code
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Errorf("never rate limited within %d attempts; last status %d. "+
			"change-password is on apiRatePerMin (300/min) rather than the "+
			"credential budget (%d/min)", authRatePerMin+2, lastCode, authRatePerMin)
	}
}

// The limiter keys on the AUTHENTICATED USER here, not the client IP.
//
// This is worth pinning because the naive expectation is wrong: register and
// login are unauthenticated and bucket by IP, while change-password runs
// behind middleware.Auth and buckets by "u:<uuid>" (see middleware.RateLimit).
// So exhausting change-password does NOT lock the same client out of login.
//
// That is the correct behaviour, not a gap. Both endpoints still enforce the
// same 5/min budget against the attacker's own identity, and keying a signed-in
// user's guessing to their IP would let one user rate-limit everyone behind a
// shared address. The oracle is closed either way; only the bucket differs.
func TestChangePasswordRateLimitIsKeyedOnTheUserNotTheIP(t *testing.T) {
	r := newCredentialRouter(t)
	tok := registerAndToken(t, r, cpEmail, cpOld)

	exhausted := false
	for i := 0; i < authRatePerMin+2; i++ {
		if rec := changePassword(t, r, tok,
			`{"current_password":"wrong guess here","new_password":"`+cpNew+`"}`); rec.Code == http.StatusTooManyRequests {
			exhausted = true
			break
		}
	}
	if !exhausted {
		t.Fatal("change-password never rate limited; it is not on the credential budget")
	}

	// A different user from the same client is unaffected, which is the
	// property that per-user keying buys.
	other := registerAndToken(t, r, "second@example.com", cpOld)
	rec := changePassword(t, r, other,
		`{"current_password":"`+cpOld+`","new_password":"`+cpNew+`"}`)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("one user exhausting the budget rate-limited a different user")
	}
}
