package accounts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// BUG A, 2026-08-05. Desktop downloads failed with
//
//	refresh provider token: oauth2: "unauthorized_client" "Unauthorized"
//
// on every Drive operation — quota sync and shard open alike — while web was
// fine.
//
// 28ba3ef fixed the EXCHANGE: the desktop connector builds a per-attempt
// config from resolveDesktopClientID/Secret and passes it into
// BeginGoogleConnectPKCE. It did not fix REFRESH, which goes through
// Service.oauth — the config stored on the service at construction.
//
// On desktop that config is built at app.go from cfg.GoogleClientID/Secret,
// the WEB environment variables, which a desktop install never sets. So
// GoogleConfigured() is false, oauthCfg is nil, and refresh has no client
// credentials at all.
//
// This test drives a refresh through the desktop config path and asserts the
// token request carries a client secret.
func TestDesktopRefreshSendsTheClientSecret(t *testing.T) {
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token request: %v", err)
		}
		forms = append(forms, r.Form)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := DesktopGoogleOAuthConfig("desktop-client-id", "desktop-client-secret",
		"http://127.0.0.1:0/callback")
	cfg.Endpoint.TokenURL = srv.URL

	// An expired token forces a refresh on the next use.
	expired := &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "the-refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Hour),
	}
	if _, err := cfg.TokenSource(context.Background(), expired).Token(); err != nil {
		t.Fatalf("refresh through the desktop config = %v", err)
	}

	if len(forms) != 1 {
		t.Fatalf("%d token requests, want 1", len(forms))
	}
	form := forms[0]
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", form.Get("grant_type"))
	}
	if got := form.Get("client_secret"); got != "desktop-client-secret" {
		t.Errorf("client_secret = %q, want it sent on refresh; "+
			"Google rejects a Desktop app client refresh without one "+
			`with oauth2: "unauthorized_client"`, got)
	}
	if got := form.Get("client_id"); got != "desktop-client-id" {
		t.Errorf("client_id = %q, want desktop-client-id", got)
	}
}

// THE ACTUAL BUG: the service used for refresh must have a config at all.
//
// A desktop install sets SKEIN_GOOGLE_DESKTOP_CLIENT_ID/_SECRET, never the web
// SKEIN_GOOGLE_CLIENT_ID/_SECRET/_REDIRECT_URL. app.go built the service's
// config only from the web triple, so on desktop the service was constructed
// with a nil config and every refresh failed before reaching Google.
func TestServiceHasARefreshConfigOnDesktop(t *testing.T) {
	svc, store, ring := newTestService(t, false) // no web oauth config
	_ = store
	_ = ring

	if svc.oauth != nil {
		t.Fatal("fixture built a service with a web config; " +
			"this test must exercise the desktop shape")
	}

	// Desktop credentials, as resolved from SKEIN_GOOGLE_DESKTOP_CLIENT_*.
	svc.SetDesktopOAuth("desktop-client-id", "desktop-client-secret")

	cfg := svc.refreshConfig()
	if cfg == nil {
		t.Fatal("refreshConfig() = nil on a desktop service; " +
			"every Drive operation fails with unauthorized_client")
	}
	if cfg.ClientID != "desktop-client-id" {
		t.Errorf("ClientID = %q, want the desktop client id", cfg.ClientID)
	}
	if cfg.ClientSecret != "desktop-client-secret" {
		t.Errorf("ClientSecret = %q; Google rejects a Desktop app refresh without it",
			cfg.ClientSecret)
	}
}

// The web build must keep using the web config, unchanged.
func TestWebRefreshConfigIsUnchanged(t *testing.T) {
	svc, _, _ := newTestService(t, true)

	cfg := svc.refreshConfig()
	if cfg == nil {
		t.Fatal("refreshConfig() = nil with a web config present")
	}
	if cfg.ClientID != "client-id" {
		t.Errorf("ClientID = %q, want the web client id", cfg.ClientID)
	}
}

// BUG B, 2026-08-05. A refresh failure happens inside the oauth2 library,
// BEFORE any Drive API call, so gdrive.apiError — which Block 3 taught to
// classify 401/403 by reason string — never runs. The account stayed 'active'
// while every operation failed, and the user saw a bare 500.
//
// The two cases need OPPOSITE responses:
//
//   - invalid_grant: the user revoked access, or the refresh token expired.
//     Reconnecting fixes it, so this is needs_reauth.
//   - unauthorized_client: the OAuth CLIENT is misconfigured. Reconnecting
//     cannot help until an operator fixes the config, and marking the account
//     needs_reauth sends the user round a loop that can never succeed.
func TestClassifyRefreshError(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		status       int
		wantSentinel error
		wantReauth   bool
	}{
		{
			name:         "invalid_grant is a dead grant",
			status:       http.StatusBadRequest,
			body:         `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`,
			wantSentinel: ErrRefreshGrantRevoked,
			wantReauth:   true,
		},
		{
			name:         "unauthorized_client is a misconfigured client",
			status:       http.StatusUnauthorized,
			body:         `{"error":"unauthorized_client","error_description":"Unauthorized"}`,
			wantSentinel: ErrRefreshClientMisconfigured,
			wantReauth:   false,
		},
		{
			name:         "invalid_client is also a config problem",
			status:       http.StatusUnauthorized,
			body:         `{"error":"invalid_client","error_description":"The OAuth client was not found."}`,
			wantSentinel: ErrRefreshClientMisconfigured,
			wantReauth:   false,
		},
		{
			name:         "a 500 from the provider is transient",
			status:       http.StatusInternalServerError,
			body:         `{"error":"server_error"}`,
			wantSentinel: nil,
			wantReauth:   false,
		},
		{
			name:         "rate limiting is transient",
			status:       http.StatusTooManyRequests,
			body:         `{"error":"rate_limit_exceeded"}`,
			wantSentinel: nil,
			wantReauth:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			cfg := DesktopGoogleOAuthConfig("id", "secret", "")
			cfg.Endpoint.TokenURL = srv.URL

			expired := &oauth2.Token{
				AccessToken:  "stale",
				RefreshToken: "rt",
				TokenType:    "Bearer",
				Expiry:       time.Now().Add(-time.Hour),
			}
			_, err := cfg.TokenSource(context.Background(), expired).Token()
			if err == nil {
				t.Fatal("the token endpoint returned an error status but Token() succeeded")
			}

			classified := classifyRefreshError(err)

			if tc.wantSentinel == nil {
				if errors.Is(classified, ErrRefreshGrantRevoked) ||
					errors.Is(classified, ErrRefreshClientMisconfigured) {
					t.Errorf("a transient failure was classified as permanent: %v", classified)
				}
			} else if !errors.Is(classified, tc.wantSentinel) {
				t.Errorf("classified = %v, want %v", classified, tc.wantSentinel)
			}

			if got := errors.Is(classified, ErrRefreshGrantRevoked); got != tc.wantReauth {
				t.Errorf("needs_reauth = %v, want %v", got, tc.wantReauth)
			}
		})
	}
}

// A misconfigured CLIENT must never mark the account needs_reauth: the user
// would reconnect, the exchange would fail the same way, and they would be
// sent round again with nothing they can do about it.
func TestRefreshFailureTransitionsOnlyOnARevokedGrant(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      error
		wantStatus string
	}{
		{"revoked grant", ErrRefreshGrantRevoked, StatusNeedsReauth},
		{"misconfigured client", ErrRefreshClientMisconfigured, StatusActive},
		{"transient", errors.New("connection reset"), StatusActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _ := newTestService(t, true)
			userID := uuid.New()
			ctx := context.Background()

			acct, err := svc.linkGoogleAccount(ctx, userID,
				&oauth2.Token{AccessToken: "tok", RefreshToken: "rt"},
				googleProfile{Sub: "sub", Email: "a@example.com"})
			if err != nil {
				t.Fatalf("link = %v", err)
			}
			stored, _ := store.GetAccount(ctx, userID, acct.ID)

			if rerr := svc.recordSyncFailure(ctx, stored, tc.cause); rerr == nil {
				t.Fatal("recordSyncFailure() = nil; it must report the cause")
			}

			after, _ := store.GetAccount(ctx, userID, acct.ID)
			if after.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", after.Status, tc.wantStatus)
			}
		})
	}
}
