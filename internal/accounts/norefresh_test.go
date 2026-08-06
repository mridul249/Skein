package accounts

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// KNOWN ISSUE #34 — AN ACCOUNT THAT CAN NEVER BE REFRESHED, AND NOTHING SAYS SO.
//
// Google returns a refresh token only on the FIRST consent for a client, or
// when prompt=consent forces re-issue. Withhold it and encryptTokens leaves the
// refresh envelope nil, which the schema accepts — refresh_token_enc is
// nullable in both dialects, deliberately, because a disconnected account keeps
// its row.
//
// The result is an account that works perfectly until the access token expires,
// typically within the hour, and then fails every Drive operation forever. It
// still reads `active` in the UI. There is no path back except noticing and
// reconnecting, and nothing tells the user to.
//
// THE FAILURE MODE IS WHY THIS IS A LAUNCH BLOCKER RATHER THAN A PAPERCUT. A
// drive that quietly stops working, with a green badge next to it, is
// indistinguishable from Skein having lost the data. That is the one thing this
// project cannot afford to look like.
//
// Detect at CREATION. The account is unrefreshable the moment it is stored, and
// that is knowable then; waiting for the first 401 means waiting for the user
// to hit it.

// An account stored without a refresh token must be born needing reauth.
func TestAnAccountWithNoRefreshTokenIsFlaggedAtCreation(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()

	acct, err := svc.linkGoogleAccount(context.Background(), userID,
		// No RefreshToken: exactly what Google returns on a re-consent that
		// does not force prompt=consent.
		&oauth2.Token{AccessToken: "ya29.access-only", Expiry: time.Now().Add(time.Hour)},
		googleProfile{Sub: "sub-no-refresh", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("linkGoogleAccount() = %v", err)
	}

	if acct.Status != StatusNeedsReauth {
		t.Errorf("status = %q, want %q.\n"+
			"Google withheld the refresh token, so this account dies silently when its "+
			"access token expires. A green badge on a drive that has already stopped "+
			"working is worse than an amber one.", acct.Status, StatusNeedsReauth)
	}

	// And it must be persisted, not merely returned — the UI reads the row.
	stored, err := store.GetAccount(context.Background(), userID, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}
	if stored.Status != StatusNeedsReauth {
		t.Errorf("persisted status = %q, want %q; the returned value was right but "+
			"the stored one is what every later read sees", stored.Status, StatusNeedsReauth)
	}
	if stored.LastError == "" {
		t.Error("no reason was recorded; the UI has nothing to explain the amber badge with")
	}
}

// THE CONVERSE, and it is what stops the fix being "mark everything amber".
func TestAnAccountWithARefreshTokenIsActive(t *testing.T) {
	svc, _, _ := newTestService(t, true)

	acct, err := svc.linkGoogleAccount(context.Background(), uuid.New(),
		&oauth2.Token{
			AccessToken:  "ya29.access",
			RefreshToken: "1//refresh",
			Expiry:       time.Now().Add(time.Hour),
		},
		googleProfile{Sub: "sub-with-refresh", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("linkGoogleAccount() = %v", err)
	}
	if acct.Status != StatusActive {
		t.Errorf("status = %q, want %q; a healthy connection must not be flagged",
			acct.Status, StatusActive)
	}
}

// RECONNECTING MUST CLEAR IT. This is the whole point of surfacing the state:
// the Reconnect button has to actually fix the thing it is offered for.
func TestReconnectingWithARefreshTokenClearsTheFlag(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	first, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "ya29.a1"},
		googleProfile{Sub: "sub-1", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("first link = %v", err)
	}
	if first.Status != StatusNeedsReauth {
		t.Fatalf("precondition: status = %q, want %q", first.Status, StatusNeedsReauth)
	}

	// The user clicks Reconnect and this time consents fully.
	again, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "ya29.a2", RefreshToken: "1//r2"},
		googleProfile{Sub: "sub-1", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("reconnect = %v", err)
	}
	if again.ID != first.ID {
		t.Fatal("reconnecting created a duplicate account")
	}
	if again.Status != StatusActive {
		t.Errorf("status = %q after a successful reconnect, want %q; "+
			"a Reconnect button that does not clear the state it is offered for "+
			"is worse than no button", again.Status, StatusActive)
	}

	stored, err := store.GetAccount(ctx, userID, first.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}
	if stored.Status != StatusActive {
		t.Errorf("persisted status = %q, want %q", stored.Status, StatusActive)
	}
	if stored.LastError != "" {
		t.Errorf("last_error = %q after a successful reconnect, want it cleared", stored.LastError)
	}
}

// A RECONNECT THAT AGAIN WITHHOLDS THE TOKEN MUST STAY FLAGGED — and must not
// lose the refresh token it already had.
//
// UpdateAccountTokens COALESCEs, so a nil envelope keeps the stored one. That
// is correct and deliberate: Google withholding a token on re-consent does not
// mean the old one stopped working. So the status must follow what the account
// ACTUALLY HOLDS after the update, not what this particular exchange returned.
func TestReconnectingWithoutARefreshTokenKeepsTheStoredOne(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	first, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "ya29.a1", RefreshToken: "1//r1"},
		googleProfile{Sub: "sub-1", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("first link = %v", err)
	}

	before, err := store.GetAccount(ctx, userID, first.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}

	// Re-consent, no refresh token returned. The stored one is untouched.
	again, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "ya29.a2"},
		googleProfile{Sub: "sub-1", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("reconnect = %v", err)
	}

	after, err := store.GetAccount(ctx, userID, first.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}
	if len(after.RefreshTokenEnc) == 0 {
		t.Fatal("the stored refresh token was destroyed by a re-consent that omitted one; " +
			"COALESCE exists precisely to prevent that")
	}
	if string(after.RefreshTokenEnc) != string(before.RefreshTokenEnc) {
		t.Error("the stored refresh envelope changed when none was supplied")
	}
	if again.Status != StatusActive {
		t.Errorf("status = %q, want %q: this account still holds a usable refresh "+
			"token, so flagging it would be a false alarm and would train the user "+
			"to ignore the badge", again.Status, StatusActive)
	}
}

// AN ALREADY-AFFECTED ACCOUNT MUST STAY FLAGGED THROUGH A SUCCESSFUL SYNC.
//
// The creation-time check does not reach accounts stored before it existed, so
// the flag has to survive the thing that runs next: the quota sync, every five
// minutes.
//
// And a successful sync is exactly what an unrefreshable account produces while
// its access token is still valid — the quota read works fine. syncAccount
// clears needs_reauth on any success, which is right for a revoked grant that
// has been re-authorised and WRONG here: it would wipe the warning within
// minutes and leave the green badge back on a drive that is still doomed.
func TestASuccessfulSyncDoesNotClearTheNoRefreshTokenFlag(t *testing.T) {
	svc, store, _ := newTestService(t, true)
	userID := uuid.New()
	ctx := context.Background()

	acct, err := svc.linkGoogleAccount(ctx, userID,
		&oauth2.Token{AccessToken: "ya29.a1", Expiry: time.Now().Add(time.Hour)},
		googleProfile{Sub: "sub-1", Email: "drive@example.com"})
	if err != nil {
		t.Fatalf("linkGoogleAccount() = %v", err)
	}
	if acct.Status != StatusNeedsReauth {
		t.Fatalf("precondition: status = %q, want %q", acct.Status, StatusNeedsReauth)
	}

	stored, err := store.GetAccount(ctx, userID, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() = %v", err)
	}

	// The decision a successful sync makes about this account's status.
	if got := statusAfterSuccessfulSync(stored); got != StatusNeedsReauth {
		t.Errorf("a successful sync would set status %q, want %q.\n"+
			"The quota read succeeds while the access token lives, so this fires "+
			"within minutes and puts a green badge back on a drive that cannot be "+
			"refreshed.", got, StatusNeedsReauth)
	}
}

// The converse: a drive that WAS revoked and has since been re-authorised must
// still go back to active on a successful sync. That is the behaviour this
// guard must not break.
func TestASuccessfulSyncStillClearsAnOrdinaryReauthFlag(t *testing.T) {
	stored := StoredAccount{
		Account:         Account{Status: StatusNeedsReauth},
		RefreshTokenEnc: []byte("a sealed refresh token"),
	}
	if got := statusAfterSuccessfulSync(stored); got != StatusActive {
		t.Errorf("status = %q, want %q; an account holding a refresh token that "+
			"syncs successfully has recovered", got, StatusActive)
	}
}

// THE CALL SITE, NOT JUST THE PREDICATE.
//
// The two tests above pin statusAfterSuccessfulSync's decision, and a mutation
// proved that is not enough: replacing the call in syncAccount with a bare
// StatusActive left both of them green, because neither reaches the call site.
// A correct function nobody calls is the same bug it was written to fix.
//
// syncAccount needs a real provider backend and OAuth config to run, which is
// a large harness for one branch. Reading the source is the cheaper honest
// check: it asserts the decision is TAKEN FROM the predicate rather than
// hardcoded, which is precisely what the mutation changed.
func TestSyncAccountAsksTheRefreshabilityPredicate(t *testing.T) {
	body, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := string(body)

	idx := strings.Index(src, "func (s *Service) syncAccount(")
	if idx < 0 {
		t.Fatal("syncAccount not found; this test is not reading what it thinks")
	}
	end := strings.Index(src[idx:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of syncAccount")
	}
	fn := src[idx : idx+end]

	if !strings.Contains(fn, "statusAfterSuccessfulSync(") {
		t.Error("syncAccount does not call statusAfterSuccessfulSync; a successful quota " +
			"read will clear needs_reauth unconditionally, wiping the no-refresh-token " +
			"warning within one five-minute tick")
	}
	if strings.Contains(fn, "SetAccountStatus(ctx, acct.ID, StatusActive") {
		t.Error("syncAccount sets StatusActive directly; the status after a successful " +
			"sync must come from statusAfterSuccessfulSync")
	}
}
