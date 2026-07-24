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
	if !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("CompleteGoogleConnect() = %v, want ErrUnauthorized", err)
	}

	for _, tc := range []struct{ state, code string }{{"", "c"}, {"s", ""}, {"", ""}} {
		if _, _, err := svc.CompleteGoogleConnect(context.Background(), tc.state, tc.code); err == nil {
			t.Errorf("CompleteGoogleConnect(%q,%q) succeeded", tc.state, tc.code)
		}
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
