package handlers

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mridul249/Skein/internal/db"
	"github.com/mridul249/Skein/internal/httpapi/httpx"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
)

// BackupTokenHeader carries the operator token.
const BackupTokenHeader = "X-Skein-Backup-Token" //nolint:gosec // header name, not a credential

// System serves operator endpoints.
//
// SECURITY MODEL, AND ITS LIMITS. The backup route returns a complete logical
// dump of the database: every user's file index, every connected Drive account,
// and the users table including password_hash. Skein has no admin role, and
// this deliberately does not invent one — it sidesteps the auth model with an
// operator token rather than extending the schema as a side effect. Two gates,
// both required:
//
//  1. A valid Skein session, as for any other API route.
//  2. The SKEIN_BACKUP_TOKEN header, compared in constant time.
//
// The session alone would be worthless: registration is open and
// unauthenticated, so anyone who can reach the instance can mint an account.
//
// When the token is unset the route reports 404, not 403. A 403 confirms the
// endpoint exists and is merely locked, which tells a scanner where to come
// back to; 404 is indistinguishable from a build without the route at all.
type System struct {
	dumper  *db.Dumper
	token   string
	sqlDB   *sql.DB
	version func() (int64, error)
	log     *slog.Logger
}

// NewSystem wires the operator endpoints. token may be empty, which disables
// the backup route entirely.
func NewSystem(dumper *db.Dumper, token string, sqlDB *sql.DB, log *slog.Logger) *System {
	return &System{dumper: dumper, token: token, sqlDB: sqlDB, log: log}
}

// Backup handles GET /api/system/backup.
func (h *System) Backup(w http.ResponseWriter, r *http.Request) {
	// Gate 1: the feature is off unless an operator turned it on.
	if h.token == "" {
		http.NotFound(w, r)
		return
	}

	// Gate 2: constant-time token comparison. A byte-by-byte compare leaks the
	// token's prefix through timing to anyone willing to measure.
	presented := r.Header.Get(BackupTokenHeader)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		h.log.WarnContext(r.Context(), "backup token rejected",
			slog.String("request_id", middleware.RequestIDFrom(r.Context())))
		httpx.WriteJSON(w, r, http.StatusForbidden, httpx.ErrorBody{
			Error:   "forbidden",
			Message: "That token is not valid.",
		})
		return
	}

	schemaVersion := int64(0)
	if h.sqlDB != nil {
		v, err := db.SchemaVersion(r.Context(), h.sqlDB)
		if err != nil {
			h.log.ErrorContext(r.Context(), "read schema version",
				slog.String("error", err.Error()))
			httpx.WriteJSON(w, r, http.StatusInternalServerError, httpx.ErrorBody{
				Error:   "internal",
				Message: "Could not determine the schema version.",
			})
			return
		}
		schemaVersion = v
	}

	// THE DUMP IS BUFFERED BEFORE ANY BYTE OF RESPONSE IS SENT.
	//
	// This is the whole lesson of 7b23f20: a dump that fails halfway must not
	// reach the client looking like a backup. Streaming straight to w would
	// mean headers (200 OK, Content-Disposition) are already committed when
	// the failure happens, and the client saves a truncated archive under a
	// filename that promises a complete one.
	//
	// The cost is holding the dump in memory. That is acceptable for a
	// self-hosted index — the shard CONTENTS are in Drive, not the database —
	// and correctness beats streaming for an operator action run a few times
	// an hour.
	var buf bytes.Buffer
	if err := h.dumper.Dump(r.Context(), &buf); err != nil {
		if errors.Is(err, db.ErrBackupBusy) {
			w.Header().Set("Retry-After", "60")
			httpx.WriteJSON(w, r, http.StatusConflict, httpx.ErrorBody{
				Error:   "backup_busy",
				Message: "A backup is already running. Try again shortly.",
			})
			return
		}
		// The provider's message goes to the log, never to the client: it can
		// carry the connection string.
		h.log.ErrorContext(r.Context(), "backup failed",
			slog.String("error", err.Error()))
		httpx.WriteJSON(w, r, http.StatusInternalServerError, httpx.ErrorBody{
			Error:   "backup_failed",
			Message: "The backup could not be completed. Nothing was written.",
		})
		return
	}

	filename := h.dumper.Filename(schemaVersion, time.Now())
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		h.log.WarnContext(r.Context(), "backup write to client failed",
			slog.String("error", err.Error()))
	}
}
