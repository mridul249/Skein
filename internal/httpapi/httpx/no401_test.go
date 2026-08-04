package httpx_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/skerr"
)

// THE RULE, ENFORCED.
//
// 401 means exactly one thing: "your Skein session is invalid, sign in again".
// The frontend acts on it — it clears the session AND retries once after a
// refresh (web/src/lib/api.ts). So any authenticated route that returns 401 for
// some other reason signs the user out and doubles the request.
//
// This has now been the same defect three times:
//
//  1. OAuth attempt failures      — fixed, accounts/service_test.go:168
//  2. A dead Google Drive grant   — fixed in Block 3b, mapped to 409
//  3. A wrong current password    — fixed 2026-08-05, mapped to a field error
//
// Each one was found in production rather than in review, which is why this
// test exists: it fails at the mapping layer, before anyone has to notice the
// symptom.
func TestOnlySessionFailuresMapTo401(t *testing.T) {
	// Every domain condition an AUTHENTICATED request can produce. If a new
	// sentinel is added and belongs here, add it — the point is that the list
	// is explicit and reviewed, not that it is inferred.
	nonSession := []struct {
		name string
		err  error
	}{
		{"validation", skerr.ErrValidation},
		{"forbidden", skerr.ErrForbidden},
		{"not found", skerr.ErrNotFound},
		{"conflict", skerr.ErrConflict},
		{"quota exceeded", skerr.ErrQuotaExceeded},
		{"too large", skerr.ErrTooLarge},
		{"rate limited", skerr.ErrRateLimited},
		{"unavailable", skerr.ErrUnavailable},
		{"not implemented", skerr.ErrNotImplemented},
		{"integrity", skerr.ErrIntegrity},
		{"drive needs reconnect", skerr.ErrDriveNeedsReconnect},
		{"provider misconfigured", skerr.ErrProviderMisconfigured},
	}

	for _, tc := range nonSession {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/anything", nil)
			httpx.WriteError(rec, req, fmt.Errorf("wrapped: %w", tc.err))

			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("%v maps to 401; the frontend will clear the user's "+
					"session and retry. 401 is reserved for an invalid Skein "+
					"session — see the rule in Memory.md", tc.err)
			}
		})
	}

	// The one condition that SHOULD be 401, so this test cannot pass by the
	// mapping having been broken in the other direction.
	t.Run("an invalid session still maps to 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		httpx.WriteError(rec, req, skerr.Public(skerr.ErrUnauthorized, "Sign in to continue."))

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 for a genuine session failure", rec.Code)
		}
	})
}

// A field-level failure on an authenticated request carries the field, so the
// form can render it inline rather than as a floating banner.
func TestFieldErrorsCarryTheirFieldAndAreNot401(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", nil)

	httpx.WriteError(rec, req, skerr.Invalid(map[string]string{
		"current_password": "That is not your current password.",
	}))

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a field validation failure returned 401")
	}

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Fields["current_password"] == "" {
		t.Errorf("fields = %v, want the offending field named", body.Fields)
	}
}
