// Package httpx renders API responses. It sits below the router so handlers
// can write typed errors without importing the package that mounts them.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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
}

// statusFor is the single place a domain error becomes an HTTP status.
// Rules.md §2.5: this mapping exists once, not per handler.
func statusFor(err error) (int, string, string) {
	switch {
	case errors.Is(err, skerr.ErrValidation):
		return http.StatusBadRequest, "validation_failed", "The request was not valid."
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
		return http.StatusInternalServerError, "internal", "Something failed on the server."
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

	WriteJSON(w, r, status, ErrorBody{
		Error:     code,
		Message:   msg,
		RequestID: reqID,
		Fields:    fields,
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
		_, _ = w.Write([]byte(`{"error":"internal","message":"Something failed on the server."}`))
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
