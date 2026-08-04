// Package skerr defines the sentinel errors the domain layer speaks in.
// Exactly one place — internal/httpapi — turns these into status codes.
package skerr

import (
	"errors"
	"fmt"
)

// The sentinel set. Wrap these with fmt.Errorf("%w") to add context; never
// return a driver or SQL error to a caller above the storage layer.
var (
	ErrValidation     = errors.New("validation failed")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrQuotaExceeded  = errors.New("quota exceeded")
	ErrTooLarge       = errors.New("too large")
	ErrRateLimited    = errors.New("rate limited")
	ErrUnavailable    = errors.New("unavailable")
	ErrNotImplemented = errors.New("not implemented")
	ErrIntegrity      = errors.New("integrity check failed")

	// ErrDriveNeedsReconnect is a connected provider account whose grant is
	// dead and which only the user can fix by re-authorising.
	//
	// It is deliberately NOT ErrUnauthorized. The frontend treats any 401 as
	// "the Skein session died" — it clears the session and retries once after
	// a refresh — so returning 401 because a *Google* grant expired would log
	// the user out of Skein for someone else's problem. This maps to 409
	// instead: the request was well-formed and the caller is who they say
	// they are, but an account is in a state that blocks it.
	ErrDriveNeedsReconnect = errors.New("drive needs reconnection")

	// ErrProviderMisconfigured is the OAuth client itself being wrong — bad
	// credentials, a deleted client, or a build missing the installed-app
	// secret needed for token refresh.
	//
	// Deliberately distinct from ErrDriveNeedsReconnect. Both mean "Drive is
	// unusable", but only one is fixable by the user: reconnecting repairs a
	// revoked grant and does nothing at all for a broken client. Rendering
	// this as needs_reauth puts a Reconnect button in front of a user who can
	// never succeed with it.
	ErrProviderMisconfigured = errors.New("provider oauth client is misconfigured")
)

// PublicError carries a message that is safe to show a user, plus optional
// per-field validation detail. It always wraps one of the sentinels above so
// the status mapping keeps working.
type PublicError struct {
	Sentinel error
	Message  string
	Fields   map[string]string
}

// Error implements error.
func (e *PublicError) Error() string {
	if e.Message == "" {
		return e.Sentinel.Error()
	}
	return fmt.Sprintf("%s: %s", e.Sentinel, e.Message)
}

// Unwrap exposes the sentinel to errors.Is.
func (e *PublicError) Unwrap() error { return e.Sentinel }

// Public builds a PublicError over sentinel with a user-facing message.
func Public(sentinel error, format string, args ...any) *PublicError {
	return &PublicError{Sentinel: sentinel, Message: fmt.Sprintf(format, args...)}
}

// Invalid builds a field-level validation error.
func Invalid(fields map[string]string) *PublicError {
	return &PublicError{
		Sentinel: ErrValidation,
		Message:  "Some fields need fixing.",
		Fields:   fields,
	}
}

// Validator accumulates field errors so a handler can report all of them at
// once instead of one round trip per mistake.
type Validator struct{ fields map[string]string }

// Check records msg against field when cond is false.
func (v *Validator) Check(cond bool, field, msg string) {
	if cond {
		return
	}
	if v.fields == nil {
		v.fields = map[string]string{}
	}
	if _, exists := v.fields[field]; !exists {
		v.fields[field] = msg
	}
}

// Err returns a validation error, or nil when every check passed.
func (v *Validator) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return Invalid(v.fields)
}
