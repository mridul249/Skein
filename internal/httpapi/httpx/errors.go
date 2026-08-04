// Package httpx renders API responses. It sits below the router so handlers
// can write typed errors without importing the package that mounts them.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mridul249/Skein/internal/files"
	"github.com/mridul249/Skein/internal/httpapi/middleware"
	"github.com/mridul249/Skein/internal/skerr"
)

// Package httpx renders API responses. It sits below the router so that
// handlers can write errors without importing the package that mounts them.

// ErrorBody is the only error shape the API ever returns.
type ErrorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	// Fields carries per-field validation messages. It is populated only
	// for validation errors and never contains internal detail.
	Fields map[string]string `json:"fields,omitempty"`
	// FileID and MissingShards describe a damaged file: shards recorded in the
	// manifest that are no longer at their provider. The indexes are included
	// so the client can name them rather than saying "something is wrong".
	FileID        string  `json:"file_id,omitempty"`
	MissingShards []int32 `json:"missing_shard_indexes,omitempty"`
	// AccountID names the connected account a drive_needs_reauth error is
	// about, so the client can badge the right drive and offer Reconnect for
	// it rather than making the user guess which of several went dead.
	AccountID string `json:"account_id,omitempty"`
}

// statusFor is the single place a domain error becomes an HTTP status.
// Rules.md §2.5: this mapping exists once, not per handler.
func statusFor(err error) (int, string, string) {
	switch {
	case errors.Is(err, skerr.ErrValidation):
		return http.StatusBadRequest, "validation_failed", "The request was not valid."
	// A misconfigured OAuth client is a SERVER fault, not a user one. It must
	// not render as needs_reauth: reconnecting cannot fix it, so a Reconnect
	// button would be a trap. 503 rather than 500 — the condition is real,
	// identified, and fixable by an operator.
	//
	// Ordered before ErrDriveNeedsReconnect so a config fault can never be
	// reported as something the user is expected to repair.
	case errors.Is(err, skerr.ErrProviderMisconfigured):
		return http.StatusServiceUnavailable, "provider_misconfigured",
			"Skein's Google client is misconfigured. This is a server setting; " +
				"reconnecting the drive will not fix it."
	// Before ErrUnauthorized: a dead Drive grant must never surface as 401.
	// The frontend clears the Skein session on any 401, so a revoked *Google*
	// token would sign the user out of the app entirely.
	// A file whose shards were deleted out of band is NOT a server error: the
	// server behaved correctly by refusing to serve a partial file. 500 tells
	// the user to retry, which can never work. 409 — the file is in a state
	// that blocks the request, and the user has a real decision to make.
	//
	// Checked before ErrIntegrity's generic mapping so the specific code wins.
	case isDamagedFile(err):
		return http.StatusConflict, "file_shards_missing",
			"Some of this file's shards are missing from their drives."
	case errors.Is(err, skerr.ErrDriveNeedsReconnect):
		return http.StatusConflict, "drive_needs_reauth",
			"A drive needs to be reconnected before this can run."
	case errors.Is(err, skerr.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized", "Authentication is required."
	case errors.Is(err, skerr.ErrForbidden):
		return http.StatusForbidden, "forbidden", "You do not have access to this."
	case errors.Is(err, skerr.ErrNotFound):
		return http.StatusNotFound, "not_found", "Not found."
	case errors.Is(err, skerr.ErrConflict):
		return http.StatusConflict, "conflict", "That conflicts with something that already exists."
	case errors.Is(err, skerr.ErrQuotaExceeded):
		return http.StatusInsufficientStorage, "quota_exceeded", "Not enough space across your drives."
	case errors.Is(err, skerr.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large", "That is larger than the configured upload limit."
	case errors.Is(err, skerr.ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited", "Too many requests. Slow down."
	case errors.Is(err, skerr.ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable", "A drive is unavailable. Try again."
	case errors.Is(err, skerr.ErrNotImplemented):
		return http.StatusNotImplemented, "not_implemented", "That is not available yet."
	default:
		return http.StatusInternalServerError, "internal", "Something failed on the server. If this persists, try reconnecting your Google Drive account.."
	}
}

// WriteError maps err to a status and writes the single error shape. Internal
// error text never reaches the client; it is logged against the request id so
// an operator can correlate the two.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := statusFor(err)
	reqID := middleware.RequestIDFrom(r.Context())

	// A user-safe message overrides the generic one, but only when the
	// domain layer explicitly marked it safe to show.
	var pub *skerr.PublicError
	var fields map[string]string
	if errors.As(err, &pub) {
		if pub.Message != "" {
			msg = pub.Message
		}
		fields = pub.Fields
	}

	// Error level is reserved for something an operator has to act on.
	//
	// A 5xx carrying a PublicError is not that: the domain layer shaped it
	// for a user, so it is an expected condition wearing a 5xx status code.
	// "No drive is connected" and "a drive filled up" are 507s that mean the
	// user should connect a drive, not that the server is broken — logging
	// them at Error fills the error log with normal traffic and trains
	// whoever reads it to ignore the level entirely.
	lg := middleware.LoggerFrom(r.Context())
	switch {
	case status >= 500 && pub == nil:
		lg.ErrorContext(r.Context(), "request failed",
			slog.String("error", err.Error()),
			slog.Int("status", status))
	case status >= 500:
		lg.WarnContext(r.Context(), "request refused",
			slog.String("error", err.Error()),
			slog.Int("status", status))
	default:
		lg.DebugContext(r.Context(), "request rejected",
			slog.String("error", err.Error()),
			slog.Int("status", status))
	}

	// account_id is promoted out of Fields to a top-level key: it identifies
	// which drive to badge, which is not a per-field validation message and
	// should not have to be dug out of a map meant for form errors.
	accountID := fields["account_id"]
	fileID := fields["file_id"]
	// Promoted out of Fields to top-level keys: these identify a resource, not
	// a form field, and should not have to be dug out of a map meant for
	// validation messages.
	if len(fields) == 1 && (accountID != "" || fileID != "") {
		fields = nil
	}

	var missingShards []int32
	if d := damagedFileFrom(err); d != nil {
		missingShards = d.MissingShards
		if fileID == "" {
			fileID = d.FileID.String()
		}
	}

	WriteJSON(w, r, status, ErrorBody{
		Error:         code,
		Message:       msg,
		RequestID:     reqID,
		Fields:        fields,
		AccountID:     accountID,
		FileID:        fileID,
		MissingShards: missingShards,
	})
}

// WriteJSON encodes v as the response body. It never writes a partial object
// with a success status: the payload is encoded first, then flushed.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		middleware.LoggerFrom(r.Context()).ErrorContext(r.Context(),
			"encode response", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal","message":"Something failed on the server. If this persists, try reconnecting your Google Drive account.."}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf)
	}
}

// WriteNoContent writes a 204.
func WriteNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// damagedFileFrom recovers the damaged-file detail from an error chain.
//
// A direct import of internal/files: there is no cycle, because files does not
// import the HTTP layer. An earlier version matched the shape through an
// interface to avoid a cycle that does not exist, which bought nothing and
// made the mapping harder to follow.
func damagedFileFrom(err error) *files.DamagedFileError {
	var d *files.DamagedFileError
	if errors.As(err, &d) {
		return d
	}
	return nil
}

func isDamagedFile(err error) bool { return damagedFileFrom(err) != nil }
