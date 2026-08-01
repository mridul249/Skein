package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
)

func newTestService(t *testing.T, withOAuth bool) (*Service, *MemoryStore, *skcrypto.Keyring) {
	t.Helper()

	master := make([]byte, skcrypto.KeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatalf("rand: %v", err)
	}
	ring, err := skcrypto.NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring() = %v", err)
	}

	var cfg *oauth2.Config
	if withOAuth {
		cfg = GoogleOAuthConfig("client-id", "client-secret", "http://localhost:8080/cb")
	}

	store := NewMemoryStore()
	svc := NewService(store, ring, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, store, ring
}

func TestBeginGoogleConnectStoresOnlyAStateHash(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()

	authURL, err := svc.BeginGoogleConnect(context.Background(), userID, "/settings")
	if err != nil {
		t.Fatalf("BeginGoogleConnect() = %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("no state in the authorisation URL")
	}

	// Only drive.file is requested. Widening this would drag the project
	// into Google's restricted-scope review and give Skein sight of files
	// it has no business seeing.
	scopes := u.Query().Get("scope")
	if !strings.Contains(scopes, "drive.file") {
		t.Errorf("scope = %q, want drive.file", scopes)
	}
	for _, forbidden := range []string{"auth/drive ", "drive.readonly", "drive.metadata"} {
		if strings.Contains(scopes, forbidden) {
			t.Errorf("scope %q includes %q", scopes, forbidden)
		}
	}

	// Offline access with forced consent, or re-connecting yields a grant
	// with no refresh token that dies within the hour.
	if u.Query().Get("access_type") != "offline" {
		t.Error("access_type is not offline; no refresh token would be issued")
	}

	// The state itself must never be stored: only its hash, so a database
	// dump contains nothing replayable.
	if store.PendingStateCount() != 1 {
		t.Fatalf("pending states = %d, want 1", store.PendingStateCount())
	}
	sum := sha256.Sum256([]byte(state))
	store.mu.Lock()
	_, hashed := store.states[string(sum[:])]
	_, raw := store.states[state]
	store.mu.Unlock()
	if !hashed {
		t.Error("the state was not stored under its SHA-256")
	}
	if raw {
		t.Error("the raw state value was stored")
	}
}

func TestBeginGoogleConnectWithoutCredentials(t *testing.T) {
	svc, _, _ := newTestService(t, false)

	_, err := svc.BeginGoogleConnect(context.Background(), uuid.New(), "/settings")
	if !errors.Is(err, skerr.ErrNotImplemented) {
		t.Fatalf("BeginGoogleConnect() = %v, want ErrNotImplemented", err)
	}
	// The message has to tell a self-hoster what to actually do.
	var pub *skerr.PublicError
	if !errors.As(err, &pub) || !strings.Contains(pub.Message, "SKEIN_GOOGLE_CLIENT_ID") {
		t.Errorf("message = %v, want it to name the missing variable", err)
	}
}

// A state is single use: a replayed callback finds nothing.
func TestOAuthStateIsSingleUseAndExpires(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()

	authURL, err := svc.BeginGoogleConnect(context.Background(), userID, "/settings")
	if err != nil {
		t.Fatalf("BeginGoogleConnect() = %v", err)
	}
	u, _ := url.Parse(authURL)
	sum := sha256.Sum256([]byte(u.Query().Get("state")))

	got, err := store.ConsumeOAuthState(context.Background(), sum[:])
	if err != nil {
		t.Fatalf("ConsumeOAuthState() = %v", err)
	}
	if got.UserID != userID {
		t.Errorf("UserID = %v, want %v", got.UserID, userID)
	}

	if _, err := store.ConsumeOAuthState(context.Background(), sum[:]); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("second consume = %v, want ErrNotFound", err)
	}

	t.Run("expired state is refused", func(t *testing.T) {
		hash := []byte("expired-state-hash")
		if err := store.CreateOAuthState(context.Background(), hash,
			PendingOAuth{UserID: userID, Kind: storage.KindGoogleDrive},
			time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("CreateOAuthState() = %v", err)
		}
		if _, err := store.ConsumeOAuthState(context.Background(), hash); !errors.Is(err, skerr.ErrNotFound) {
			t.Errorf("expired consume = %v, want ErrNotFound", err)
		}
	})
}

func TestCompleteGoogleConnectRejectsUnknownState(t *testing.T) {
	svc, _, _ := newTestService(t, true)

	_, _, err := svc.CompleteGoogleConnect(context.Background(), "never-issued", "code")
	// ErrValidation, not ErrUnauthorized: see the comment on this branch in
	// service.go. An unknown OAuth state says nothing about the caller's own
	// Skein session.
	if !errors.Is(err, skerr.ErrValidation) {
		t.Fatalf("CompleteGoogleConnect() = %v, want ErrValidation", err)
	}

	for _, tc := range []struct{ state, code string }{{"", "c"}, {"s", ""}, {"", ""}} {
		if _, _, err := svc.CompleteGoogleConnect(context.Background(), tc.state, tc.code); err == nil {
			t.Errorf("CompleteGoogleConnect(%q,%q) succeeded", tc.state, tc.code)
		}
	}
}

// A real bug, reproduced 2026-08-01: BeginGoogleConnect on the desktop
// build runs behind middleware.Auth, and the frontend's request layer
// treats *any* 401 response as "the Skein session died" — it clears the
// whole app session and, worse, retries the failed request once against a
// freshly refreshed token (api.ts's one-retry-after-refresh rule), which
// for a connect attempt means a second loopback listener and a second
// browser tab. An OAuth exchange failure is about the attempt, never about
// whether the caller is still logged into Skein, so none of the failure
// paths reachable from that authenticated handler may map to
// skerr.ErrUnauthorized. This guards the whole class, not just the one
// call site the bug was first seen on.
func TestOAuthAttemptFailuresNeverMapToUnauthorized(t *testing.T) {
	svc, _, _ := newTestService(t, true)

	_, _, err := svc.CompleteGoogleConnect(context.Background(), "never-issued", "code")
	if errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("CompleteGoogleConnect(unknown state) = %v; must never be ErrUnauthorized "+
			"(the frontend clears the session and retries on any 401)", err)
	}
}

// Rules.md §2.9 and §2.11: tokens are stored as versioned ciphertext, salted
// per user, and a token written for one user cannot be read as another's.
func TestTokensAreStoredAsEncryptedEnvelopes(t *testing.T) {
	svc, store, ring := newTestService(t, true)
	userID := uuid.New()

	const accessTok = "ya29.this-is-an-access-token"
	const refreshTok = "1//this-is-a-refresh-token"

	acct, err := svc.linkGoogleAccount(context.Background(), userID,
		&oauth2.Token{
			AccessToken:  accessTok,
			RefreshToken: refreshTok,
			Expiry:       time.Now().Add(time.Hour),
		},
		googleProfile{Sub: "google-sub-1", Email: "drive@example.com", Name: "Drive One"})
	if err != nil {
		t.Fatalf("linkGoogleAccount() = %v", err)
	}

	stored, err := store.GetAccount(context.Background(), userID, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}

	// Nothing recognisable is at rest.
	if bytes.Contains(stored.AccessTokenEnc, []byte(accessTok)) {
		t.Error("the access token is stored in plaintext")
	}
	if bytes.Contains(stored.RefreshTokenEnc, []byte(refreshTok)) {
		t.Error("the refresh token is stored in plaintext")
	}

	// Every ciphertext carries a version byte and the key id.
	if stored.AccessTokenEnc[0] != skcrypto.EnvelopeV1 {
		t.Errorf("version byte = %d, want %d", stored.AccessTokenEnc[0], skcrypto.EnvelopeV1)
	}
	id := ring.KeyID()
	if !bytes.Equal(stored.AccessTokenEnc[1:1+len(id)], id[:]) {
		t.Error("the key id is missing from the stored envelope")
	}

	// And it round-trips.
	creds, err := svc.credentialsFor(stored)
	if err != nil {
		t.Fatalf("credentialsFor() = %v", err)
	}
	if creds.AccessToken != accessTok || creds.RefreshToken != refreshTok {
		t.Error("credentials did not round-trip")
	}

	// The salt is the user id, so the same ciphertext read as another user
	// fails rather than silently decrypting.
	stolen := stored
	stolen.UserID = uuid.New()
	if _, err := svc.credentialsFor(stolen); err == nil {
		t.Error("a token decrypted under the wrong user's salt")
	}
}

// Rules.md §2.4: identity is the provider's account id, never the email.
func TestLinkingIsKeyedOnProviderIDNotEmail(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	first, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "a1", RefreshToken: "r1"},
		googleProfile{Sub: "sub-one", Email: "shared@example.com"})
	if err != nil {
		t.Fatalf("link first = %v", err)
	}

	// A different Google account that happens to present the same address
	// must become a separate drive, not silently take over the first.
	second, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "a2", RefreshToken: "r2"},
		googleProfile{Sub: "sub-two", Email: "shared@example.com"})
	if err != nil {
		t.Fatalf("link second = %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two distinct Google accounts were merged on email")
	}

	// Re-connecting the same provider id updates in place instead of
	// creating a duplicate.
	again, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "a3", RefreshToken: "r3"},
		googleProfile{Sub: "sub-one", Email: "renamed@example.com"})
	if err != nil {
		t.Fatalf("re-link = %v", err)
	}
	if again.ID != first.ID {
		t.Error("re-connecting an account created a duplicate")
	}
	if again.Email != "renamed@example.com" {
		t.Errorf("email = %q, want the updated address", again.Email)
	}

	all, err := store.ListAccounts(ctx, userID)
	if err != nil {
		t.Fatalf("ListAccounts() = %v", err)
	}
	if len(all) != 2 {
		t.Errorf("accounts = %d, want 2", len(all))
	}
}

func TestOrdinalsAreAssignedInConnectionOrder(t *testing.T) {
	svc, _, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	var got []int32
	for i, sub := range []string{"a", "b", "c"} {
		acct, err := svc.linkGoogleAccount(ctx, userID,
			&oauth2.Token{AccessToken: "tok"},
			googleProfile{Sub: sub, Email: sub + "@example.com"})
		if err != nil {
			t.Fatalf("link %d = %v", i, err)
		}
		got = append(got, acct.Ordinal)
	}

	// The ordinal is the account's position in the colour ramp, which is
	// its identity across the whole interface — it must be stable and
	// distinct.
	for i, o := range got {
		if o != int32(i+1) {
			t.Errorf("account %d has ordinal %d, want %d", i, o, i+1)
		}
	}
}

func TestAccountsAreScopedToTheirOwner(t *testing.T) {
	svc, _, _ := newTestService(t, true)
	owner, stranger := uuid.New(), uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, owner,
		&oauth2.Token{AccessToken: "tok"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}

	if _, gerr := svc.store.GetAccount(ctx, stranger, acct.ID); !errors.Is(gerr, skerr.ErrNotFound) {
		t.Errorf("a stranger read another user's account: %v", gerr)
	}
	if derr := svc.Disconnect(ctx, stranger, acct.ID); !errors.Is(derr, skerr.ErrNotFound) {
		t.Errorf("a stranger deleted another user's account: %v", derr)
	}
	if _, oerr := svc.store.GetAccount(ctx, owner, acct.ID); oerr != nil {
		t.Errorf("the owner's account was affected: %v", oerr)
	}

	list, err := svc.List(ctx, stranger)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a stranger listed %d accounts, want 0", len(list))
	}
}

func TestDisconnect(t *testing.T) {
	svc, _, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}

	if err := svc.Disconnect(ctx, userID, acct.ID); err != nil {
		t.Fatalf("Disconnect() = %v", err)
	}
	if err := svc.Disconnect(ctx, userID, acct.ID); !errors.Is(err, skerr.ErrNotFound) {
		t.Errorf("second Disconnect() = %v, want ErrNotFound", err)
	}
}

// Known issue #19: Disconnect soft deletes so file_shards.connected_account_id
// survives (ON DELETE SET NULL would otherwise orphan every shard on the drive,
// unrecoverably). The row therefore outlives the disconnect, and these are the
// consequences that must hold for that to be safe.
func TestDisconnectDisablesTheRowWithoutDeletingIt(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok", RefreshToken: "refresh"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}

	if derr := svc.Disconnect(ctx, userID, acct.ID); derr != nil {
		t.Fatalf("Disconnect() = %v", derr)
	}

	// The row — and with it the id every shard points at — must still exist.
	stored, gerr := store.GetAccount(ctx, userID, acct.ID)
	if gerr != nil {
		t.Fatalf("account row is gone after Disconnect: %v; shard links would be orphaned", gerr)
	}
	if stored.Status != StatusDisabled {
		t.Errorf("status = %q, want %q", stored.Status, StatusDisabled)
	}
	// A surviving row must not keep working credentials.
	if len(stored.AccessTokenEnc) != 0 || len(stored.RefreshTokenEnc) != 0 {
		t.Error("disconnected account still holds OAuth tokens")
	}
	// And must not resolve to a usable backend. Without this the row's
	// survival would mean a disconnected drive still served files.
	if _, berr := svc.Backend(ctx, userID, acct.ID); berr == nil {
		t.Error("Backend() succeeded for a disconnected drive")
	} else if !errors.Is(berr, skerr.ErrUnavailable) {
		t.Errorf("Backend() = %v, want ErrUnavailable", berr)
	}
}

// The payoff for the soft delete: reconnecting the same Google identity must
// land on the SAME row id, because that id is what every shard still points at.
// A new row would leave the files unreadable forever.
func TestReconnectReusesTheSameAccountRow(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()
	profile := googleProfile{Sub: "sub", Email: "a@example.com"}

	acct, err := svc.linkGoogleAccount(ctx, userID, &oauth2.Token{AccessToken: "tok"}, profile)
	if err != nil {
		t.Fatalf("link = %v", err)
	}
	if derr := svc.Disconnect(ctx, userID, acct.ID); derr != nil {
		t.Fatalf("Disconnect() = %v", derr)
	}

	again, rerr := svc.linkGoogleAccount(ctx, userID, &oauth2.Token{AccessToken: "tok2"}, profile)
	if rerr != nil {
		t.Fatalf("reconnect = %v", rerr)
	}
	if again.ID != acct.ID {
		t.Errorf("reconnect produced a new row %s, want the original %s; every shard link would be dead",
			again.ID, acct.ID)
	}

	// Reconnecting has to restore usability, not just the row.
	stored, gerr := store.GetAccount(ctx, userID, acct.ID)
	if gerr != nil {
		t.Fatalf("GetAccount after reconnect = %v", gerr)
	}
	if stored.Status != StatusActive {
		t.Errorf("status after reconnect = %q, want %q", stored.Status, StatusActive)
	}
	if _, berr := svc.Backend(ctx, userID, acct.ID); berr != nil {
		t.Errorf("Backend() after reconnect = %v, want a usable backend", berr)
	}
}

// The soft-deleted row must not read as a connected drive. List is what the
// drives page renders, so a disabled row left in it would show a drive the
// user disconnected as though it were still there.
//
// PoolFor is deliberately different — it keeps disabled rows so the quota UI
// can explain a drive that is not contributing (see
// TestDisabledAccountsAreExcludedFromThePool) — but their bytes must still be
// out of the totals, which is what this also pins down.
func TestDisconnectedDrivesDisappearFromListings(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	keep, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"}, googleProfile{Sub: "keep", Email: "keep@example.com"})
	if err != nil {
		t.Fatalf("link keep = %v", err)
	}
	drop, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"}, googleProfile{Sub: "drop", Email: "drop@example.com"})
	if err != nil {
		t.Fatalf("link drop = %v", err)
	}
	for _, id := range []uuid.UUID{keep.ID, drop.ID} {
		if uerr := store.UpsertCapacity(ctx, id, 100, 10); uerr != nil {
			t.Fatalf("UpsertCapacity() = %v", uerr)
		}
	}

	if derr := svc.Disconnect(ctx, userID, drop.ID); derr != nil {
		t.Fatalf("Disconnect() = %v", derr)
	}

	listed, err := svc.List(ctx, userID)
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != keep.ID {
		t.Errorf("List() returned %d account(s), want only %s", len(listed), keep.ID)
	}

	// PoolFor still reports the row, but the disconnected drive's bytes must
	// not count toward the pool.
	pool, err := svc.PoolFor(ctx, userID)
	if err != nil {
		t.Fatalf("PoolFor() = %v", err)
	}
	if pool.TotalBytes != 100 {
		t.Errorf("pool total = %d, want 100 (only the connected drive)", pool.TotalBytes)
	}
}

// Resolver caches backends and consults that cache before re-reading the
// account, so a disconnect that only changes the row would go unnoticed there.
// Disconnect must actively invalidate it.
func TestDisconnectInvalidatesCachedBackends(t *testing.T) {
	svc, _, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"}, googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}

	var forgotten []uuid.UUID
	svc.OnAccountInvalidated(func(id uuid.UUID) { forgotten = append(forgotten, id) })

	if derr := svc.Disconnect(ctx, userID, acct.ID); derr != nil {
		t.Fatalf("Disconnect() = %v", derr)
	}
	if len(forgotten) != 1 || forgotten[0] != acct.ID {
		t.Errorf("invalidated = %v, want exactly [%s]; a stale cached backend would keep serving the drive",
			forgotten, acct.ID)
	}
}

func TestPoolAggregatesCapacity(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	var ids []uuid.UUID
	for _, sub := range []string{"a", "b", "c"} {
		acct, err := svc.linkGoogleAccount(ctx, userID,
			&oauth2.Token{AccessToken: "tok"},
			googleProfile{Sub: sub, Email: sub + "@example.com"})
		if err != nil {
			t.Fatalf("link = %v", err)
		}
		ids = append(ids, acct.ID)
	}

	const fifteenGiB = 15 << 30
	for i, id := range ids {
		used := int64(i) * (1 << 30)
		if err := store.UpsertCapacity(ctx, id, fifteenGiB, used); err != nil {
			t.Fatalf("UpsertCapacity() = %v", err)
		}
	}
	// One in-flight reservation, which must reduce reported free space.
	store.SetReserved(ids[0], 2<<30)

	pool, err := svc.PoolFor(ctx, userID)
	if err != nil {
		t.Fatalf("PoolFor() = %v", err)
	}

	if pool.TotalBytes != 3*fifteenGiB {
		t.Errorf("TotalBytes = %d, want %d", pool.TotalBytes, int64(3*fifteenGiB))
	}
	if pool.UsedBytes != 3<<30 {
		t.Errorf("UsedBytes = %d, want %d", pool.UsedBytes, int64(3<<30))
	}

	// Free is total - used - reserved: an upload may not claim bytes that
	// another upload has already committed to.
	wantFree := int64(3*fifteenGiB) - (3 << 30) - (2 << 30)
	if pool.FreeBytes != wantFree {
		t.Errorf("FreeBytes = %d, want %d", pool.FreeBytes, wantFree)
	}
}

func TestCapacityFreeBytesNeverNegative(t *testing.T) {
	c := Capacity{TotalBytes: 100, UsedBytes: 80, ReservedBytes: 50}
	if got := c.FreeBytes(); got != 0 {
		t.Errorf("FreeBytes() = %d, want 0", got)
	}
}

func TestDisabledAccountsAreExcludedFromThePool(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}
	if err := store.UpsertCapacity(ctx, acct.ID, 15<<30, 0); err != nil {
		t.Fatalf("UpsertCapacity() = %v", err)
	}
	if err := store.SetAccountStatus(ctx, acct.ID, StatusDisabled, "turned off"); err != nil {
		t.Fatalf("SetAccountStatus() = %v", err)
	}

	pool, err := svc.PoolFor(ctx, userID)
	if err != nil {
		t.Fatalf("PoolFor() = %v", err)
	}
	if pool.TotalBytes != 0 || pool.FreeBytes != 0 {
		t.Errorf("a disabled drive still counted toward the pool: %+v", pool)
	}
	// It is still listed, so the UI can explain why it is not contributing.
	if len(pool.Accounts) != 1 {
		t.Errorf("drives listed = %d, want 1", len(pool.Accounts))
	}
}

func TestPurgeExpiredOAuthStates(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	ctx := context.Background()
	userID := uuid.New()

	if err := store.CreateOAuthState(ctx, []byte("fresh"),
		PendingOAuth{UserID: userID}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateOAuthState() = %v", err)
	}
	if err := store.CreateOAuthState(ctx, []byte("stale"),
		PendingOAuth{UserID: userID}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CreateOAuthState() = %v", err)
	}

	n, err := svc.PurgeExpiredOAuthStates(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredOAuthStates() = %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if store.PendingStateCount() != 1 {
		t.Errorf("remaining = %d, want 1", store.PendingStateCount())
	}
}

func TestSyncMarksARevokedGrantForReauth(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}

	stored, err := store.GetAccount(ctx, userID, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}
	if err := svc.recordSyncFailure(ctx, stored, storage.ErrUnauthorized); err == nil {
		t.Fatal("recordSyncFailure() returned nil; it must report the cause")
	}

	after, err := store.GetAccount(ctx, userID, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}
	if after.Status != StatusNeedsReauth {
		t.Errorf("status = %q, want %q", after.Status, StatusNeedsReauth)
	}
	if !strings.Contains(after.LastError, "Reconnect") {
		t.Errorf("last_error = %q, want an actionable message", after.LastError)
	}
}

func TestSyncKeepsAccountActiveOnATransientError(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "tok"},
		googleProfile{Sub: "sub", Email: "a@example.com"})
	if err != nil {
		t.Fatalf("link = %v", err)
	}
	stored, _ := store.GetAccount(ctx, userID, acct.ID)

	// A network blip must not demand that the user re-authorise.
	if err := svc.recordSyncFailure(ctx, stored, errors.New("connection reset")); err == nil {
		t.Fatal("recordSyncFailure() returned nil")
	}
	after, _ := store.GetAccount(ctx, userID, acct.ID)
	if after.Status != StatusActive {
		t.Errorf("status = %q, want %q for a transient failure", after.Status, StatusActive)
	}
}
