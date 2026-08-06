package accounts

import (
	"context"
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
