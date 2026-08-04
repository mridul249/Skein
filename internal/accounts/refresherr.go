package accounts

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
)

// Refresh failures happen inside the oauth2 library, before any Drive API call
// is made. gdrive.apiError — which classifies 401/403 by reason string — never
// runs on this path, so the account stayed 'active' while every operation
// failed and the user saw a bare 500. These sentinels close that gap.
//
// The two permanent cases need OPPOSITE responses, which is the whole reason
// they are distinguished rather than collapsed into one "auth failed".
var (
	// ErrRefreshGrantRevoked means the user revoked Skein's access, or the
	// refresh token expired. RECONNECTING FIXES IT, so it maps to
	// needs_reauth.
	ErrRefreshGrantRevoked = errors.New("accounts: the provider grant was revoked or expired")

	// ErrRefreshClientMisconfigured means the OAuth CLIENT is wrong — bad
	// credentials, a deleted client, or a desktop build with no installed-app
	// secret wired for refresh (the 2026-08-05 bug).
	//
	// This must NOT mark the account needs_reauth. Reconnecting cannot help
	// until an operator fixes the configuration, so a Reconnect button here
	// sends the user round a loop that can never succeed. It is a server
	// configuration error and is reported as one.
	ErrRefreshClientMisconfigured = errors.New("accounts: the oauth client is misconfigured")
)

// classifyRefreshError maps a token-refresh failure to one of the sentinels
// above, or returns it unchanged when it is transient.
//
// Classification is on the OAuth error CODE (RFC 6749 §5.2), not the HTTP
// status. Google returns 400 for invalid_grant and 401 for
// unauthorized_client, but the status alone cannot separate "the user revoked
// this" from "your client is broken" — and getting that backwards is what
// produces a reconnect loop.
//
// Anything unrecognised is left transient on purpose: an unknown failure must
// not disable a working account. The cost of being wrong in that direction is
// a retry; the cost in the other direction is a user re-authorising for no
// reason.
func classifyRefreshError(err error) error {
	if err == nil {
		return nil
	}

	var rerr *oauth2.RetrieveError
	if !errors.As(err, &rerr) {
		// Not an OAuth protocol error at all — a dial failure, a timeout, a
		// cancelled context. Transient by definition.
		return err
	}

	switch rerr.ErrorCode {
	case "invalid_grant":
		return fmt.Errorf("%w: %s", ErrRefreshGrantRevoked, rerr.ErrorCode)

	case "unauthorized_client", "invalid_client":
		return fmt.Errorf("%w: %s", ErrRefreshClientMisconfigured, rerr.ErrorCode)

	default:
		// server_error, temporarily_unavailable, rate limits, and anything
		// Google adds later.
		return err
	}
}

// AsAPIError converts a refresh failure into the structured error the API
// returns, naming the account so the client can badge the right drive.
//
// The two permanent classes map to DIFFERENT sentinels on purpose:
//
//   - a revoked grant becomes ErrDriveNeedsReconnect, rendered as 409 with an
//     account_id and a Reconnect affordance;
//   - a misconfigured client becomes ErrProviderMisconfigured, rendered as 503
//     with no Reconnect button, because reconnecting cannot fix it.
//
// 409 rather than 401 for the first is load-bearing: the frontend treats any
// 401 as "the Skein session died" and clears it, so a dead *Google* grant
// returning 401 would log the user out of Skein entirely.
//
// A transient failure is returned unchanged and keeps whatever status it
// already mapped to.
func AsAPIError(err error, accountID uuid.UUID, email string) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, ErrRefreshGrantRevoked), errors.Is(err, storage.ErrUnauthorized):
		return &skerr.PublicError{
			Sentinel: skerr.ErrDriveNeedsReconnect,
			Message: fmt.Sprintf(
				"Google access for %s has been revoked or expired. Reconnect that drive to continue.",
				displayName(email)),
			Fields: map[string]string{"account_id": accountID.String()},
		}

	case errors.Is(err, ErrRefreshClientMisconfigured):
		// Deliberately carries NO account_id: the fault is not with this
		// account, and attaching one invites the UI to badge a drive that is
		// perfectly fine.
		return &skerr.PublicError{
			Sentinel: skerr.ErrProviderMisconfigured,
			Message: "Skein's Google client is misconfigured, so drive access " +
				"cannot be refreshed. This is a server setting; reconnecting will not fix it.",
		}
	}
	return err
}

// displayName falls back to something renderable when the account has no
// recorded email.
func displayName(email string) string {
	if email == "" {
		return "this drive"
	}
	return email
}
