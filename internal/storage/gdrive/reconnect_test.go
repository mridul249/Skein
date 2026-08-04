package gdrive

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mridul249/Skein/internal/storage"
)

// The whole point of the sentinel: a dead grant is distinguishable from a
// throttle, and both are distinguishable from running out of space.
func TestNeedsReconnectDistinguishesDeadGrantsFromTransientFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"revoked grant", storage.ErrUnauthorized, true},
		{"explicit sentinel", ErrDriveNeedsReconnect, true},
		{"wrapped reconnect error", NewReconnectError("acct-1", "a@b.co", storage.ErrUnauthorized), true},

		// The cases that must NOT demand re-authorisation.
		{"rate limited", storage.ErrRateLimited, false},
		{"out of space", storage.ErrQuota, false},
		{"object missing", storage.ErrObjectNotFound, false},
		{"network blip", errors.New("connection reset by peer"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsReconnect(tc.err); got != tc.want {
				t.Errorf("NeedsReconnect(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The account id has to survive wrapping, or the API cannot say WHICH drive
// the user needs to reconnect.
func TestReconnectErrorCarriesTheAccountID(t *testing.T) {
	base := NewReconnectError("acct-42", "drive@example.com", storage.ErrUnauthorized)
	wrapped := fmt.Errorf("quota sync: %w", base)

	var re *ReconnectError
	if !errors.As(wrapped, &re) {
		t.Fatal("errors.As could not recover the ReconnectError through a wrap")
	}
	if re.AccountID != "acct-42" {
		t.Errorf("AccountID = %q, want acct-42", re.AccountID)
	}
	if re.Email != "drive@example.com" {
		t.Errorf("Email = %q, want drive@example.com", re.Email)
	}
	// Both halves of the multi-unwrap still match through the outer wrap.
	if !errors.Is(wrapped, ErrDriveNeedsReconnect) {
		t.Error("wrapped error no longer matches ErrDriveNeedsReconnect")
	}
	if !errors.Is(wrapped, storage.ErrUnauthorized) {
		t.Error("wrapped error lost its underlying cause")
	}
}
