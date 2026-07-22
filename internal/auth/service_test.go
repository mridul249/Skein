package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mridul60214/skein/internal/skerr"
)

const (
	testEmail    = "mridul@example.com"
	testPassword = "correct horse battery staple"
)

func newTestService(t *testing.T) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	svc := NewService(
		store,
		NewTokenIssuer(strings.Repeat("k", 48), 15*time.Minute),
		720*time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return svc, store
}

func testMeta() RequestMeta {
	return RequestMeta{IP: ParseIP("198.51.100.5"), UserAgent: "skein-test/1.0"}
}

func TestRegisterAndLogin(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("Register returned an empty token")
	}
	if pair.User.Email != testEmail {
		t.Errorf("email = %q, want %q", pair.User.Email, testEmail)
	}

	logged, err := svc.Login(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if logged.RefreshToken == pair.RefreshToken {
		t.Error("a second login reused the first refresh token")
	}
	if logged.User.ID != pair.User.ID {
		t.Error("login returned a different user")
	}
}

func TestRegisterNormalisesEmail(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, "  Mridul@Example.COM ", testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if pair.User.Email != testEmail {
		t.Errorf("stored email = %q, want %q", pair.User.Email, testEmail)
	}

	// The same address in a different case must not create a second account.
	if _, err := svc.Register(ctx, "MRIDUL@EXAMPLE.COM", testPassword, testMeta()); !errors.Is(err, skerr.ErrConflict) {
		t.Errorf("duplicate register = %v, want ErrConflict", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		password string
		wantErr  error
	}{
		{"empty email", "", testPassword, skerr.ErrValidation},
		{"malformed email", "not-an-email", testPassword, skerr.ErrValidation},
		{"email with display name", "Mridul <m@example.com>", testPassword, skerr.ErrValidation},
		{"short password", testEmail, "short", skerr.ErrValidation},
		{"overlong password", testEmail, strings.Repeat("x", 300), skerr.ErrValidation},
		{"overlong email", strings.Repeat("a", 250) + "@example.com", testPassword, skerr.ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t)
			_, err := svc.Register(context.Background(), tc.email, tc.password, testMeta())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Register() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLoginWrongPassword(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Register(ctx, testEmail, testPassword, testMeta()); err != nil {
		t.Fatalf("Register() = %v", err)
	}

	_, err := svc.Login(ctx, testEmail, "not the right password", testMeta())
	if !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("Login() = %v, want ErrUnauthorized", err)
	}

	// The message must not distinguish a wrong password from an unknown
	// account, or it becomes a user-enumeration oracle.
	var pub *skerr.PublicError
	if !errors.As(err, &pub) {
		t.Fatal("expected a PublicError")
	}
	if !strings.Contains(pub.Message, "Email or password") {
		t.Errorf("message = %q, want the ambiguous form", pub.Message)
	}

	if got := len(store.EventsOfKind(EventLoginFailed)); got != 1 {
		t.Errorf("login.failed events = %d, want 1", got)
	}
}

func TestLoginUnknownUserIsIndistinguishable(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Login(context.Background(), "nobody@example.com", testPassword, testMeta())
	if !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("Login() = %v, want ErrUnauthorized", err)
	}
	var pub *skerr.PublicError
	if !errors.As(err, &pub) || !strings.Contains(pub.Message, "Email or password") {
		t.Errorf("unknown user produced a distinguishable error: %v", err)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	second, err := svc.Refresh(ctx, first.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh returned the same token; it must rotate")
	}
	if second.SessionID == first.SessionID {
		t.Fatal("refresh reused the session row")
	}

	// The successor records where it came from, and stays in the family.
	next, ok := store.SessionByID(second.SessionID)
	if !ok {
		t.Fatal("successor session was not stored")
	}
	if next.PrevID == nil || *next.PrevID != first.SessionID {
		t.Errorf("prev_id = %v, want %v", next.PrevID, first.SessionID)
	}
	prev, _ := store.SessionByID(first.SessionID)
	if next.FamilyID != prev.FamilyID {
		t.Error("rotation started a new family; it must inherit one")
	}
	if prev.UsedAt == nil {
		t.Error("the presented token was not marked used")
	}

	// Chained rotation keeps working.
	third, err := svc.Refresh(ctx, second.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("second Refresh() = %v", err)
	}
	if third.RefreshToken == second.RefreshToken {
		t.Fatal("second rotation did not rotate")
	}
}

// The Phase 1 exit criterion, and the rule the reference project broke:
// presenting an already-used refresh token revokes the entire family.
func TestRefreshReuseRevokesFamily(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	second, err := svc.Refresh(ctx, first.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
	third, err := svc.Refresh(ctx, second.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	// An attacker replays the first token, which the legitimate client
	// already spent two rotations ago.
	if _, err := svc.Refresh(ctx, first.RefreshToken, testMeta()); !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("replay = %v, want ErrUnauthorized", err)
	}

	family, _ := store.SessionByID(first.SessionID)
	for _, s := range store.SessionsInFamily(family.FamilyID) {
		if s.RevokedAt == nil {
			t.Errorf("session %s survived the reuse; the whole family must be revoked", s.ID)
		}
	}

	// The still-current token, held by the legitimate client, is dead too.
	// That is the point: the server cannot tell victim from attacker.
	if _, err := svc.Refresh(ctx, third.RefreshToken, testMeta()); !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("post-revocation refresh = %v, want ErrUnauthorized", err)
	}

	events := store.EventsOfKind(EventRefreshReuse)
	if len(events) == 0 {
		t.Fatal("no refresh.reuse_detected security event was recorded")
	}
	if events[0].Detail["reason"] != "token_reused" {
		t.Errorf("reason = %v, want token_reused", events[0].Detail["reason"])
	}
}

// Two requests presenting the same token concurrently: exactly one may win,
// and the loser must be treated as reuse.
func TestRefreshConcurrentUseRevokesFamily(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rerr := svc.Refresh(ctx, first.RefreshToken, testMeta()); rerr == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want exactly 1", okCount)
	}

	family, _ := store.SessionByID(first.SessionID)
	for _, s := range store.SessionsInFamily(family.FamilyID) {
		if s.RevokedAt == nil {
			t.Errorf("session %s survived a concurrent-use race", s.ID)
		}
	}
}

func TestRefreshRejectsUnknownAndEmptyTokens(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	for _, tok := range []string{"", "not-a-real-token"} {
		if _, err := svc.Refresh(ctx, tok, testMeta()); !errors.Is(err, skerr.ErrUnauthorized) {
			t.Errorf("Refresh(%q) = %v, want ErrUnauthorized", tok, err)
		}
	}
	// An unknown token has no family, so nothing is revoked — but it is
	// still recorded.
	if len(store.EventsOfKind(EventRefreshRejected)) == 0 {
		t.Error("an unknown refresh token was not recorded")
	}
}

func TestRefreshRejectsExpiredToken(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	if err := store.ExpireSession(pair.SessionID); err != nil {
		t.Fatalf("ExpireSession() = %v", err)
	}

	if _, err := svc.Refresh(ctx, pair.RefreshToken, testMeta()); !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("Refresh() = %v, want ErrUnauthorized", err)
	}
}

// The family deadline must not slide, or a stolen chain lives forever.
func TestRefreshDoesNotExtendTheFamilyDeadline(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	original, _ := store.SessionByID(first.SessionID)

	second, err := svc.Refresh(ctx, first.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}
	rotated, _ := store.SessionByID(second.SessionID)

	if !rotated.ExpiresAt.Equal(original.ExpiresAt) {
		t.Errorf("expiry moved from %v to %v; rotation must inherit the deadline",
			original.ExpiresAt, rotated.ExpiresAt)
	}
}

func TestLogoutRevokesTheFamily(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	first, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}
	second, err := svc.Refresh(ctx, first.RefreshToken, testMeta())
	if err != nil {
		t.Fatalf("Refresh() = %v", err)
	}

	if err := svc.Logout(ctx, second.RefreshToken, testMeta()); err != nil {
		t.Fatalf("Logout() = %v", err)
	}
	if _, err := svc.Refresh(ctx, second.RefreshToken, testMeta()); !errors.Is(err, skerr.ErrUnauthorized) {
		t.Fatalf("refresh after logout = %v, want ErrUnauthorized", err)
	}

	family, _ := store.SessionByID(first.SessionID)
	for _, s := range store.SessionsInFamily(family.FamilyID) {
		if s.RevokedAt == nil {
			t.Errorf("session %s survived logout", s.ID)
		}
	}

	// Logging out twice, or with a token nobody has ever seen, is fine.
	if err := svc.Logout(ctx, second.RefreshToken, testMeta()); err != nil {
		t.Errorf("second Logout() = %v, want nil", err)
	}
	if err := svc.Logout(ctx, "", testMeta()); err != nil {
		t.Errorf("Logout(\"\") = %v, want nil", err)
	}
}

func TestMe(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	got, err := svc.Me(ctx, pair.User.ID)
	if err != nil {
		t.Fatalf("Me() = %v", err)
	}
	if got.Email != testEmail {
		t.Errorf("email = %q, want %q", got.Email, testEmail)
	}
}

func TestLoginUpgradesAWeakHash(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()

	pair, err := svc.Register(ctx, testEmail, testPassword, testMeta())
	if err != nil {
		t.Fatalf("Register() = %v", err)
	}

	// Simulate a hash written when the parameters were lower.
	weak := "$argon2id$v=19$m=4096,t=1,p=1$" +
		"c2FsdHNhbHRzYWx0c2FsdA$" +
		mustWeakKey(t, testPassword)
	if err := store.UpdateUserPassword(ctx, pair.User.ID, weak); err != nil {
		t.Fatalf("UpdateUserPassword() = %v", err)
	}
	if !NeedsRehash(weak) {
		t.Fatal("NeedsRehash() = false for weaker parameters")
	}

	if _, err := svc.Login(ctx, testEmail, testPassword, testMeta()); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	upgraded, _ := store.GetUserByID(ctx, pair.User.ID)
	if NeedsRehash(upgraded.PasswordHash) {
		t.Error("login did not upgrade the stored hash")
	}
}
