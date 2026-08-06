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

	skcrypto "github.com/mridul249/Skein/internal/crypto"
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
	dumper   *db.Dumper
	token    string
	sqlDB    *sql.DB
	keyring  *skcrypto.Keyring
	backfill ManifestBackfiller
	coverage ManifestBackfiller
	log      *slog.Logger
}

// SetKeyring wires the master keyring for the key-export route. Nil leaves
// that route reporting 404, exactly as an unset token does.
func (h *System) SetKeyring(k *skcrypto.Keyring) { h.keyring = k }

// ExportKey handles GET /api/system/key-export.
//
// THIS ROUTE RETURNS THE SINGLE SECRET THAT DECRYPTS EVERY FILE IN THE
// INSTANCE. It exists because SKEIN_MASTER_KEY otherwise has no backup story
// at all: lose it and every shard is permanently unreadable, and sidecar
// manifests make that worse by removing the database as the authoritative
// record. Redundancy is not disaster recovery until the key can be kept
// somewhere the instance is not.
//
// The gate is exactly the backup route's, and deliberately nothing more
// elaborate: the operator token on top of a session, constant-time compared,
// 404 when unset. There is NO other way in — no query parameter, no bypass,
// no debug flag. The token gate is the only thing standing between a caller
// and the key, which is why it must not acquire a second path later.
//
// Nothing here is enumerable. There is one key per instance, so the route
// takes no identifier and the response introduces none; sequential ids are
// what made an earlier exposure walkable (issue #43).
func (h *System) ExportKey(w http.ResponseWriter, r *http.Request) {
	// Gate 1: off unless an operator turned it on, and off if no keyring is
	// wired. Both report 404 — an unavailable feature must be
	// indistinguishable from one that was never built.
	if h.token == "" || h.keyring == nil {
		http.NotFound(w, r)
		return
	}

	// Gate 2: constant-time comparison, as for the backup route.
	presented := r.Header.Get(BackupTokenHeader)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		// The key id is NOT logged here. It is not secret, but a refused
		// request should tell the caller and the log nothing about the
		// instance it failed against.
		h.log.WarnContext(r.Context(), "key export token rejected",
			slog.String("request_id", middleware.RequestIDFrom(r.Context())))
		httpx.WriteJSON(w, r, http.StatusForbidden, httpx.ErrorBody{
			Error:   "forbidden",
			Message: "That token is not valid.",
		})
		return
	}

	// An audit line, because an export is a genuinely significant event: it is
	// the moment the key leaves the process. The key id identifies WHICH
	// instance was exported and is derived, so it reveals nothing about the
	// key. The key itself never appears here or anywhere else.
	h.log.WarnContext(r.Context(), "master key exported",
		slog.String("request_id", middleware.RequestIDFrom(r.Context())),
		slog.String("key_id", h.keyring.KeyIDString()))

	file := ExportKeyFileFor(h.keyring)

	// no-store, not merely no-cache: the key must not reach a disk cache, a
	// proxy, or the back/forward cache. An attachment rather than inline, so
	// no browser renders it into a tab that lands in history.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		`attachment; filename="skein-master-key-`+h.keyring.KeyIDString()+`.txt"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(file)))
	w.WriteHeader(http.StatusOK)

	// HEAD carries no body by contract, and some clients issue it
	// speculatively. Never hand the key to one.
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(file); err != nil {
		// Deliberately no key material and no detail beyond the fact.
		h.log.WarnContext(r.Context(), "key export interrupted",
			slog.String("request_id", middleware.RequestIDFrom(r.Context())))
	}
}

// ExportKeyFileFor renders the key file. Split out so the route stays readable
// and so the rendering can be exercised directly.
func ExportKeyFileFor(k *skcrypto.Keyring) []byte { return skcrypto.ExportKeyFile(k) }

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
