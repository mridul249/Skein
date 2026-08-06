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
	ManifestCoverageForUser(ctx context.Context, userID uuid.UUID) (files.BackfillReport, error)
}

// SetBackfiller wires manifest backfill. Nil leaves the route reporting 404,
// exactly as an unset operator token does.
func (h *System) SetBackfiller(b ManifestBackfiller) {
	h.backfill = b
	h.coverage = b
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
	if h.token == "" || h.backfill == nil {
		http.NotFound(w, r)
		return
	}
	presented := r.Header.Get(BackupTokenHeader)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
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

	report, berr := h.backfill.BackfillManifestsForUser(r.Context(), userID)
	if berr != nil {
		httpx.WriteError(w, r, berr)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, report)
}
