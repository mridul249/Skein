package handlers

import (
	"net/http"

	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// recoveryStatusResponse is what the Recovery settings section renders.
type recoveryStatusResponse struct {
	// KeyID identifies WHICH master key this instance is running under. Shown
	// so an operator can compare it against the `Key ID:` line in an exported
	// key file.
	//
	// NOT SECRET. It is a 4-byte HKDF output over the master key and already
	// travels in the clear in every ciphertext envelope, so displaying it
	// discloses nothing that reading one stored byte would not.
	KeyID string `json:"key_id"`

	// BackupTokenSet says whether the operator token is configured, so the UI
	// can explain why Backfill is unavailable instead of showing a button that
	// 404s.
	BackupTokenSet bool `json:"backup_token_set"`

	// ManifestsWritable says whether this build can write manifests without an
	// operator token — true on desktop, where the person clicking the button
	// is the operator. The UI uses it to decide whether to ask for a token at
	// all, rather than showing a field nobody can fill in.
	ManifestsWritable bool `json:"manifests_writable"`

	// ManifestsWritableWithoutToken is true on the desktop build. The UI uses
	// it to decide whether to show a token field at all, rather than asking
	// for a credential the install has no reason to have.
	ManifestsWritableWithoutToken bool `json:"manifests_writable_without_token"`
}

// RecoveryStatus handles GET /api/system/recovery.
//
// SESSION-GATED, NOT OPERATOR-TOKEN-GATED, and the difference is deliberate.
// The routes behind the operator token either export a secret or write to
// every connected drive. This one reads two non-secret facts, and putting it
// behind the token would mean the Recovery UI could not render at all for an
// operator who has not set one — which is exactly the person most likely to
// need to be told that recovery depends on a key file they may not have.
//
// THERE IS NO ENDPOINT THAT ACCEPTS A KEY, and there must never be. The master
// key has to be in the environment before startup: key_id verification refuses
// to boot on a mismatch, and every subsystem derives from the keyring during
// wiring. A UI field accepting a key would be taking it after everything that
// needs it has already run.
func (h *System) RecoveryStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := middleware.MustUserID(r.Context()); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	out := recoveryStatusResponse{
		BackupTokenSet:                h.token != "",
		ManifestsWritable:             h.manifestsOpen || h.token != "",
		ManifestsWritableWithoutToken: h.manifestsOpen,
	}
	if h.keyring != nil {
		out.KeyID = h.keyring.KeyIDString()
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// ManifestCoverage handles GET /api/system/manifests/coverage.
//
// READ-ONLY: it lists each drive and counts manifests, and writes nothing.
// Asking "am I recoverable?" must not mutate storage — a user checking their
// safety net should not be surprised by writes to four Drive accounts.
//
// Session-gated for the same reason as RecoveryStatus: it reveals counts of
// the caller's own files, which they can already list.
func (h *System) ManifestCoverage(w http.ResponseWriter, r *http.Request) {
	if h.coverage == nil {
		http.NotFound(w, r)
		return
	}
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	report, cerr := h.coverage.ManifestCoverageForUser(r.Context(), userID)
	if cerr != nil {
		httpx.WriteError(w, r, cerr)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, report)
}
