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

// A dead Google grant must NOT surface as 401.
//
// The frontend's request layer treats any 401 as "the Skein session died": it
// clears the session and retries once after a refresh. Mapping a revoked
// *Google* token to 401 would therefore sign the user out of Skein because a
// third party revoked something unrelated to their Skein login.
func TestDriveNeedsReconnectIsNotA401(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)

	httpx.WriteError(rec, req, fmt.Errorf("sync: %w", skerr.ErrDriveNeedsReconnect))

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a dead Drive grant returned 401; the frontend will clear the Skein session")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "drive_needs_reauth" {
		t.Errorf("error code = %q, want drive_needs_reauth", body.Error)
	}
	if body.Message == "" {
		t.Error("no message for the user")
	}
}

// The response names WHICH drive, so the client can badge it and offer
// Reconnect without guessing among several connected accounts.
func TestDriveNeedsReconnectCarriesTheAccountID(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)

	httpx.WriteError(rec, req, &skerr.PublicError{
		Sentinel: skerr.ErrDriveNeedsReconnect,
		Message:  "Reconnect drive@example.com to continue.",
		Fields:   map[string]string{"account_id": "acct-42"},
	})

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccountID != "acct-42" {
		t.Errorf("account_id = %q, want acct-42", body.AccountID)
	}
	if body.Message != "Reconnect drive@example.com to continue." {
		t.Errorf("message = %q, want the domain layer's message", body.Message)
	}
	// Promoted out of Fields, not duplicated into it.
	if _, dup := body.Fields["account_id"]; dup {
		t.Error("account_id appears in both fields and the top level")
	}
}
