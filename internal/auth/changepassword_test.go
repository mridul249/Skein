package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/skerr"
)

const newTestPassword = "a different sufficiently long password"

// The happy path: the right current password is accepted, and the new one is
// what works from then on.
func TestChangePasswordReplacesTheCredential(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	if _, cerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}

	if _, lerr := svc.Login(ctx, testEmail, newTestPassword, testMeta()); lerr != nil {
		t.Errorf("Login() with the new password = %v, want success", lerr)
	}
	if _, lerr := svc.Login(ctx, testEmail, testPassword, testMeta()); lerr == nil {
		t.Error("the old password still logs in after a change")
	}
}

// The wrong current password must not change anything. Without this check the
// endpoint is an account takeover for anyone holding a stolen session.
func TestChangePasswordRejectsAWrongCurrentPassword(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	before, _ := store.GetUserByID(ctx, pair.User.ID)

	_, cerr := svc.ChangePassword(ctx, pair.User.ID, "not the current password", newTestPassword, testMeta())
	if cerr == nil {
		t.Fatal("ChangePassword() accepted a wrong current password")
	}
	// ErrValidation, never ErrUnauthorized: the caller's session is fine, one
	// field of their input was not. See
	// TestChangePasswordWrongCurrentIsNotUnauthorized.
	if !errors.Is(cerr, skerr.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", cerr)
	}

	// The stored hash is untouched, and the original password still works.
	after, _ := store.GetUserByID(ctx, pair.User.ID)
	if after.PasswordHash != before.PasswordHash {
		t.Error("a rejected change still rewrote the password hash")
	}
	if _, lerr := svc.Login(ctx, testEmail, testPassword, testMeta()); lerr != nil {
		t.Errorf("the original password stopped working after a rejected change: %v", lerr)
	}
}

// The new password goes through the same validator registration uses. Reusing
// it rather than writing a second one is the point: two validators drift, and
// the weaker one becomes the real policy.
func TestChangePasswordAppliesTheRegistrationValidator(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	short := strings.Repeat("a", minPasswordLen-1)
	_, cerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, short, testMeta())
	if cerr == nil {
		t.Fatalf("ChangePassword() accepted a %d-rune password; the minimum is %d",
			len(short), minPasswordLen)
	}
	if !errors.Is(cerr, skerr.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", cerr)
	}

	// And a password that registration would reject for length at the top end
	// is rejected here too, by the same rule.
	long := strings.Repeat("b", maxPasswordLen+1)
	if _, lerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, long, testMeta()); lerr == nil {
		t.Error("ChangePassword() accepted a password over the maximum length")
	}
}

// The rehash uses the current argon2id parameters, so a changed password is
// not left at weaker settings than a fresh registration would produce.
func TestChangePasswordRehashesAtCurrentParameters(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if _, cerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}

	user, _ := store.GetUserByID(ctx, pair.User.ID)
	if NeedsRehash(user.PasswordHash) {
		t.Error("the new hash already needs rehashing; it was not written at current parameters")
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$") {
		t.Errorf("hash = %q, want an argon2id PHC string", user.PasswordHash)
	}
}

// A password change is a security event. EventPasswordChanged was declared and
// never emitted (known issue #18's note); this is its first caller.
func TestChangePasswordRecordsASecurityEvent(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if _, cerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}

	events := store.EventsOfKind(EventPasswordChanged)
	if len(events) != 1 {
		t.Fatalf("%d %s events, want 1", len(events), EventPasswordChanged)
	}
	if events[0].UserID == nil || *events[0].UserID != pair.User.ID {
		t.Error("the event is not attributed to the user who changed their password")
	}
}

// A failed attempt is also worth recording, and must not be attributed as a
// successful change.
func TestChangePasswordDoesNotRecordSuccessOnFailure(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	_, _ = svc.ChangePassword(ctx, pair.User.ID, "wrong", newTestPassword, testMeta())

	if events := store.EventsOfKind(EventPasswordChanged); len(events) != 0 {
		t.Errorf("%d %s events after a REJECTED change, want 0",
			len(events), EventPasswordChanged)
	}
}

// The gap this file used to pin is CLOSED. Known issue #18's per-user epoch
// landed with the schema bundle, so a password change now signs other devices
// out, and TestChangePasswordDoesNotYetRevokeOtherSessions was inverted rather
// than deleted — it lives on as TestPasswordChangeRevokesOtherSessions in
// epoch_race_test.go, alongside the concurrency case that is the actual reason
// RevokeAllUserSessions could not be used.
//
// Kept as a note rather than dropped silently, because the old test's doc
// comment told the next reader to expect a failure here and to invert it. This
// says the inversion happened and where it went.

// SKEIN_MASTER_KEY is independent of the login password: no argon2id output
// reaches any file key. That is what makes a password change safe to perform
// at all — if the two were coupled, changing a password would strand every
// encrypted file and every stored OAuth token.
//
// Asserted here rather than assumed, so it stays true. The keyring derives
// from the master secret, a salt and an info string (crypto/kdf.go Derive);
// the password is not an input to any of them.
func TestChangePasswordLeavesMasterKeyDerivationUnchanged(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	master := make([]byte, skcrypto.KeyLen)
	if _, rerr := rand.Read(master); rerr != nil {
		t.Fatalf("rand: %v", rerr)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	salt := pair.User.ID[:]

	// Every purpose-separated key, captured before the change.
	infos := []string{
		skcrypto.InfoToken, skcrypto.InfoFile, skcrypto.InfoShare,
		skcrypto.InfoOAuth, skcrypto.InfoCapability,
	}
	before := make(map[string][]byte, len(infos))
	for _, info := range infos {
		key, derr := ring.Derive(info, salt)
		if derr != nil {
			t.Fatalf("Derive(%s) = %v", info, derr)
		}
		before[info] = key
	}

	// A real ciphertext, to prove decryptability survives and not merely that
	// the bytes match.
	sealed, err := ring.SealString(skcrypto.InfoFile, salt, "file contents")
	if err != nil {
		t.Fatalf("SealString() = %v", err)
	}

	if _, cerr := svc.ChangePassword(ctx, pair.User.ID, testPassword, newTestPassword, testMeta()); cerr != nil {
		t.Fatalf("ChangePassword() = %v", cerr)
	}

	for _, info := range infos {
		after, derr := ring.Derive(info, salt)
		if derr != nil {
			t.Fatalf("Derive(%s) after change = %v", info, derr)
		}
		if !bytes.Equal(before[info], after) {
			t.Errorf("%s key changed when the password changed; "+
				"file access is coupled to the login credential", info)
		}
	}

	opened, err := ring.OpenString(skcrypto.InfoFile, salt, sealed)
	if err != nil {
		t.Fatalf("data sealed before the password change no longer opens: %v", err)
	}
	if opened != "file contents" {
		t.Errorf("decrypted %q, want %q", opened, "file contents")
	}
}

// BUG, 2026-08-05. A WRONG CURRENT PASSWORD MUST NOT RETURN 401.
//
// This is the third instance of the same defect class, after the OAuth-attempt
// failures (service_test.go:168) and the dead Drive grant (Block 3b). The
// frontend treats ANY 401 as "the Skein session died": it clears the session
// and, worse, retries once after a refresh — so a typo in the current-password
// box signs the user out before the error can render, and spends two of the
// 5/min credential budget doing it.
//
// A wrong current password is a failed field validation on an authenticated
// request. The caller's session is perfectly valid; one field of their input
// was not.
func TestChangePasswordWrongCurrentIsNotUnauthorized(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	_, cerr := svc.ChangePassword(ctx, pair.User.ID, "not the current password", newTestPassword, testMeta())
	if cerr == nil {
		t.Fatal("ChangePassword() accepted a wrong current password")
	}

	if errors.Is(cerr, skerr.ErrUnauthorized) {
		t.Fatalf("wrong current password returned ErrUnauthorized (%v); "+
			"the frontend clears the Skein session on any 401, so the user is "+
			"signed out instead of seeing the error", cerr)
	}
	if !errors.Is(cerr, skerr.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", cerr)
	}

	// And it names the field, so the modal can render it inline rather than as
	// a floating banner.
	var pub *skerr.PublicError
	if !errors.As(cerr, &pub) {
		t.Fatal("the error carries no public message")
	}
	if pub.Fields["current_password"] == "" {
		t.Errorf("fields = %v, want a current_password message", pub.Fields)
	}
}
