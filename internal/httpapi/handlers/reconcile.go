package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
	"github.com/mridul249/Skein/internal/skerr"
)

// Reconcile handles POST /api/system/reconcile.
//
// SYNCHRONOUS, returning a summary. Not streamed over the Block 6 SSE
// machinery, and that is a measured decision rather than a default: the work
// is bounded by the shared Drive pool at 4 concurrent metadata calls, each a
// ranged single-byte GET, over at most maxBulkFiles files. That is seconds,
// not minutes — and a progress stream for something that finishes before a
// spinner is worth showing is machinery without a job.
//
// The threshold to revisit: if a listing ever exceeds a few hundred files, or
// the per-shard check becomes more than one cheap call, this should move to
// the SSE path — which exists and is proven, so the change is small.
func (h *Files) Reconcile(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	report, rerr := h.svc.Reconcile(r.Context(), userID)
	if rerr != nil {
		httpx.WriteError(w, r, rerr)
		return
	}
	// 200 even when the run is incomplete: the request succeeded, and the
	// report says what it could and could not establish. An error status would
	// throw away the partial results the client needs.
	httpx.WriteJSON(w, r, http.StatusOK, report)
}

// Reconstruct handles POST /api/system/reconstruct.
//
// Rebuilds database rows from the sidecar manifests on the user's drives. The
// recovery path for a lost database: the shards are still in Drive, and this
// reads back the record of what they belong to.
//
// ADDITIVE ONLY, so it is safe to run against a live database — it inserts
// what is missing and touches nothing that exists. That is what makes it
// idempotent and what keeps it distinct from reconcile, which is the operation
// allowed to conclude something is gone.
//
// 200 even when the run is incomplete, for the same reason Reconcile does: the
// request succeeded, and the report says which drives could not be scanned. An
// error status would discard the partial recovery the client just got.
func (h *Files) Reconstruct(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	report, rerr := h.svc.ReconstructAll(r.Context(), userID)
	if rerr != nil {
		httpx.WriteError(w, r, rerr)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, report)
}

// PurgeDamaged handles DELETE /api/files/{id}/damaged.
//
// Its own route rather than a flag on the ordinary delete: it destroys data,
// it refuses anything it cannot confirm is damaged, and giving it a distinct
// path means it cannot be reached by accident from the normal delete UI.
func (h *Files) PurgeDamaged(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	fileID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		httpx.WriteError(w, r, skerr.Invalid(map[string]string{
			"id": "That is not a valid file id.",
		}))
		return
	}

	if derr := h.svc.PurgeDamaged(r.Context(), userID, fileID); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	httpx.WriteNoContent(w)
}
