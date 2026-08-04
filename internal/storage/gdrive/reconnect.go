package gdrive

import (
	"errors"
	"fmt"

	"github.com/mridul249/Skein/internal/storage"
)

// ErrDriveNeedsReconnect reports a Drive account whose grant is dead: the
// token has been revoked, expired beyond refresh, or the user removed Skein's
// access from their Google account settings. The only fix is re-authorisation
// by the user, which is what distinguishes it from every other Drive failure.
//
// It is deliberately NOT returned for a 403 rate limit. Drive reports a
// revoked grant, a storage-quota refusal and a throttle all as 403,
// distinguishable only by the reason string in the body — see apiError. A rate
// limit is transient, and treating it as a dead grant would walk the user
// through a pointless re-consent after which the next burst would do it again.
var ErrDriveNeedsReconnect = errors.New("gdrive: account needs reconnection")

// NeedsReconnect reports whether err means the user must re-authorise.
//
// storage.ErrUnauthorized is included because that is what apiError already
// returns for a revoked grant, and accounts.recordSyncFailure has keyed the
// needs_reauth transition on it since before this sentinel existed. Both are
// accepted so the two paths cannot disagree about what "dead grant" means.
func NeedsReconnect(err error) bool {
	return errors.Is(err, ErrDriveNeedsReconnect) ||
		errors.Is(err, storage.ErrUnauthorized)
}

// ReconnectError pairs the sentinel with the account the user has to fix, so
// the API can answer {code, message, account_id} rather than making the client
// guess which of several connected drives went dead.
type ReconnectError struct {
	AccountID string
	Email     string
	cause     error
}

// NewReconnectError marks cause as needing re-authorisation of one account.
func NewReconnectError(accountID, email string, cause error) *ReconnectError {
	return &ReconnectError{AccountID: accountID, Email: email, cause: cause}
}

func (e *ReconnectError) Error() string {
	return fmt.Sprintf("gdrive: account %s needs reconnection: %v", e.AccountID, e.cause)
}

// Unwrap exposes both the underlying cause and the sentinel, so errors.Is
// matches ErrDriveNeedsReconnect and whatever storage error caused it.
func (e *ReconnectError) Unwrap() []error {
	return []error{ErrDriveNeedsReconnect, e.cause}
}
