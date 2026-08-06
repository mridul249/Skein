package handlers

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// ManifestBackfiller is the slice of files.Service this route needs.
//
// An interface so the system handler group does not take a dependency on the
// whole files service for one route.
type ManifestBackfiller interface {
	BackfillManifestsForUser(ctx context.Context, userID uuid.UUID) (files.BackfillReport, error)
	RewriteManifestsForUser(ctx context.Context, userID uuid.UUID) (files.BackfillReport, error)
	ManifestCoverageForUser(ctx context.Context, userID uuid.UUID) (files.BackfillReport, error)
}

// SetBackfiller wires manifest backfill. Nil leaves the route reporting 404,
// exactly as an unset operator token does.
func (h *System) SetBackfiller(b ManifestBackfiller) {
	h.backfill = b
	h.coverage = b
}

// AllowWithoutOperatorToken drops the operator-token requirement for the
// manifest routes.
//
// SET ONLY ON THE DESKTOP BUILD, and the reasoning is about who the "operator"
// is rather than about convenience. The token exists because the server build
// is reachable by anyone who can hit the port, registration is open, and the
// gated routes either export a secret or write to every connected drive. On
// the desktop build the server binds 127.0.0.1 on a random port inside the
// user's own session, and the person clicking the button IS the operator —
// there is no second party the token protects them from.
//
// Requiring it there had a concrete cost: a desktop install has no reason to
// set SKEIN_BACKUP_TOKEN, so the Recovery UI could show only an explanation of
// why it could not act, and repairing manifests needed a separate command-line
// tool. That is a bad procedure for something a user runs during a disaster.
//
// The key-export and database-dump routes keep the token on BOTH builds: those
// hand over a secret, and the reasoning above does not extend to them.
func (h *System) AllowWithoutOperatorToken() { h.manifestsOpen = true }

// manifestsGateOpen reports whether the manifest routes may run for this
// request. Either the operator token matched, or this is a desktop build where
// the token is not required.
func (h *System) manifestsGateOpen(r *http.Request) bool {
	if h.manifestsOpen {
		return true
	}
	if h.token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(BackupTokenHeader)), []byte(h.token)) == 1
}

// BackfillManifests handles POST /api/system/manifests/backfill.
//
// WHY IT IS GATED LIKE THE BACKUP ROUTE. It writes to every connected drive,
// once per file per account, through the shared provider pool. That is a large
// amount of outbound traffic on someone else's rate budget, triggered by one
// request — so it takes the operator token on top of a session, exactly as the
// dump and the key export do, and reports 404 rather than 403 when the token
// is unset so an unavailable feature is indistinguishable from one that was
// never built.
//
// 200 even when the run is incomplete, matching reconcile and reconstruct: the
// request succeeded and the report says which drives could not be reached.
// An error status would discard the coverage the client just earned.
func (h *System) BackfillManifests(w http.ResponseWriter, r *http.Request) {
	if h.backfill == nil {
		http.NotFound(w, r)
		return
	}
	if !h.manifestsGateOpen(r) {
		if h.token == "" {
			http.NotFound(w, r)
			return
		}
		h.log.WarnContext(r.Context(), "manifest backfill token rejected",
			slog.String("request_id", middleware.RequestIDFrom(r.Context())))
		httpx.WriteJSON(w, r, http.StatusForbidden, httpx.ErrorBody{
			Error:   "forbidden",
			Message: "That token is not valid.",
		})
		return
	}

	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// ?rewrite=true regenerates manifests that already exist. Needed after a
	// format change: a manifest missing a field cannot be repaired from the
	// drives, only rewritten from the database, and ordinary backfill skips it
	// precisely because a manifest is already there.
	report, berr := h.backfill.BackfillManifestsForUser(r.Context(), userID)
	if r.URL.Query().Get("rewrite") == "true" {
		report, berr = h.backfill.RewriteManifestsForUser(r.Context(), userID)
	}
	if berr != nil {
		httpx.WriteError(w, r, berr)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, report)
}
