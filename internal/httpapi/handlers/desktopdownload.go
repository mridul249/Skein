//go:build desktop

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
	"github.com/mridul249/Skein/internal/skerr"
)

// heartbeatInterval keeps the SSE connection distinguishable from a dead one.
//
// Without it a stalled transfer and a broken connection look identical: both
// are silence. A comment frame every 15s means the client can treat a missing
// heartbeat as "the connection died" and re-attach, while a heartbeat with an
// unchanged byte count means "the transfer is stuck" — genuinely different
// conditions needing different responses.
const heartbeatInterval = 15 * time.Second

// DesktopDownloads serves the Go-side download path.
//
// DESKTOP BUILD ONLY, enforced by the build tag on this file rather than by a
// runtime check. The Start route writes to a path on the machine running the
// server; on a hosted deployment that is not the caller's machine.
type DesktopDownloads struct {
	mgr        *files.DownloadManager
	defaultDir func() string
}

// NewDesktopDownloads wires the handler. defaultDir resolves the configured
// download directory at call time, so a Settings change takes effect without a
// restart.
func NewDesktopDownloads(mgr *files.DownloadManager, defaultDir func() string) *DesktopDownloads {
	return &DesktopDownloads{mgr: mgr, defaultDir: defaultDir}
}

type startDownloadRequest struct {
	FileID string `json:"file_id"`
	// Dir overrides the configured folder, for "Save as…". Empty uses the
	// default.
	Dir string `json:"dir,omitempty"`
}

// Capability handles GET /api/desktop/capabilities.
//
// The probe the frontend uses to decide whether it is running in the desktop
// shell. Present only in the desktop binary, so a 404 in the browser build is
// the correct and only answer — the client falls back to the a.click() path.
func (h *DesktopDownloads) Capability(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{
		"desktop_downloads": true,
		"download_dir":      h.defaultDir(),
	})
}

// Start handles POST /api/desktop/downloads.
func (h *DesktopDownloads) Start(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var req startDownloadRequest
	if derr := decodeJSON(r, &req); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	fileID, perr := uuid.Parse(req.FileID)
	if perr != nil {
		httpx.WriteError(w, r, skerr.Invalid(map[string]string{
			"file_id": "That is not a valid file id.",
		}))
		return
	}

	// The configured root and the REQUESTED directory are passed separately
	// and never collapsed: the root is trusted configuration, req.Dir is
	// caller-supplied and must be confined inside it.

	// Every precondition — ownership, shard reachability, and that the target
	// directory exists and is writable — is checked inside Start, before a
	// byte moves or a file is created.
	dl, serr := h.mgr.Start(r.Context(), userID, fileID, h.defaultDir(), req.Dir)
	if serr != nil {
		httpx.WriteError(w, r, serr)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, dl)
}

// List handles GET /api/desktop/downloads.
func (h *DesktopDownloads) List(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"downloads": h.mgr.List(userID)})
}

// Cancel handles DELETE /api/desktop/downloads/{id}.
func (h *DesktopDownloads) Cancel(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.mgr.Cancel(userID, chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteNoContent(w)
}

// Events handles GET /api/desktop/downloads/{id}/events.
//
// Server-sent events rather than Wails EventsEmit: the desktop window
// navigates to a real http:// origin (see third_party/wails-v2.13.0/PATCH.md),
// which means Wails skips registering its asset server and the window.runtime
// bridge does not exist. SSE over the server the window is already a client of
// needs no bridge at all.
func (h *DesktopDownloads) Events(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// http.ResponseController, NOT a w.(http.Flusher) type assertion.
	//
	// The middleware chain wraps the writer (middleware.statusRecorder, for
	// the access log), and a direct assertion does not follow the Unwrap chain
	// — so it failed and every SSE request 501'd. Found by running it, not by
	// review. ResponseController is exactly the mechanism statusRecorder's
	// Unwrap exists for.
	rc := http.NewResponseController(w)

	updates, unsubscribe, found := h.mgr.Subscribe(userID, chi.URLParam(r, "id"))
	if !found {
		httpx.WriteError(w, r, skerr.ErrNotFound)
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point of streaming these.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Flush after the header, not before: flushing first would commit a 200
	// and make an error response impossible. A flush failure here means the
	// writer cannot stream, and the client sees a connection that closes
	// rather than a stream that never updates.
	if err := rc.Flush(); err != nil {
		return
	}

	beat := time.NewTicker(heartbeatInterval)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The client went away. The TRANSFER IS NOT CANCELLED — it is
			// owned by the manager, not by this connection, so a dropped
			// EventSource leaves the download running and the client
			// re-attaches by subscribing again.
			return

		case snap, open := <-updates:
			if !open {
				return
			}
			payload, err := json.Marshal(snap)
			if err != nil {
				return
			}
			if _, werr := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", payload); werr != nil {
				return
			}
			_ = rc.Flush()

			// Terminal states end the stream: there is nothing further to
			// report, and holding the connection open would look like a
			// transfer still running.
			if snap.State != files.DownloadRunning {
				return
			}

		case <-beat.C:
			// A comment frame. Distinguishes "still connected, nothing new"
			// from a dead connection — without it the two are both silence.
			if _, werr := fmt.Fprint(w, ": heartbeat\n\n"); werr != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}
