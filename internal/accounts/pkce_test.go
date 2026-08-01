package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/mridul60214/skein/internal/skerr"
)

// recordingTokenServer stands in for Google's token endpoint. It records the
// full form of every request and always issues a token — the tests below
// inspect what was sent rather than have the server itself gate on it.
func recordingTokenServer(t *testing.T) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token request: %v", err)
		}
		forms = append(forms, r.Form)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access-token",
			"refresh_token": "fake-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &forms
}

func testPKCEConfig(tokenURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: "desktop-client-id",
		// Desktop app clients do carry a secret: Google issues one and
		// requires it at exchange. It is not confidential (RFC 8252 §8.5)
		// but it must be sent. See accounts.DesktopGoogleOAuthConfig.
		ClientSecret: "desktop-client-secret",
		RedirectURL:  "http://127.0.0.1:0/callback",
		Scopes:       []string{"drive.file"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/auth",
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// The client secret belongs at the token endpoint, never in the
// authorisation URL — that URL is handed to the system browser, lands in
// history, and is visible in the address bar. Google requires the secret at
// exchange (see DesktopGoogleOAuthConfig) but never in this leg, so a config
// that carries one must still produce a clean auth URL.
func TestBeginGoogleConnectPKCENeverPutsTheSecretInTheAuthURL(t *testing.T) {
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig("https://example.invalid/token")
	if cfg.ClientSecret == "" {
		t.Fatal("test config has no client secret; this test cannot show one is withheld")
	}

	verifier := oauth2.GenerateVerifier()
	authURL, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, verifier, uuid.New(), "")
	if err != nil {
		t.Fatalf("BeginGoogleConnectPKCE() = %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if u.Query().Get("client_secret") != "" {
		t.Error("auth URL carries a client_secret; it belongs only in the token exchange")
	}
	if strings.Contains(authURL, cfg.ClientSecret) {
		t.Errorf("auth URL %q contains the client secret", authURL)
	}
	wantChallenge := s256(verifier)
	if got := u.Query().Get("code_challenge"); got != wantChallenge {
		t.Errorf("code_challenge = %q, want %q", got, wantChallenge)
	}
	if got := u.Query().Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
}

// The exit criterion: PKCE verifier is generated per attempt and never
// reused. Two attempts by the same caller must produce two distinct
// verifiers and two distinct challenges.
func TestPKCEVerifierIsNeverReusedAcrossAttempts(t *testing.T) {
	svc, store, _ := newTestService(t, false)
	cfg := testPKCEConfig("https://example.invalid/token")
	userID := uuid.New()

	v1 := oauth2.GenerateVerifier()
	if _, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, v1, userID, ""); err != nil {
		t.Fatalf("first BeginGoogleConnectPKCE() = %v", err)
	}
	v2 := oauth2.GenerateVerifier()
	if _, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, v2, userID, ""); err != nil {
		t.Fatalf("second BeginGoogleConnectPKCE() = %v", err)
	}

	if v1 == v2 {
		t.Fatal("oauth2.GenerateVerifier produced the same verifier twice; test is not exercising anything")
	}
	if store.PendingStateCount() != 2 {
		t.Fatalf("pending states = %d, want 2", store.PendingStateCount())
	}

	// Both verifiers must actually have been persisted (so a stolen state
	// hash cannot be completed with an attacker-supplied verifier), and they
	// must be the two distinct values generated above, not a shared one.
	store.mu.Lock()
	seen := make(map[string]bool)
	for _, st := range store.states {
		seen[st.pending.PKCEVerifier] = true
	}
	store.mu.Unlock()
	if !seen[v1] || !seen[v2] {
		t.Fatal("both per-attempt verifiers should be present in stored state")
	}
	if len(seen) != 2 {
		t.Fatalf("distinct stored verifiers = %d, want 2", len(seen))
	}
}

// CompleteGoogleConnectPKCE must present the verifier stored server-side to
// the token endpoint, never one carried on the request — proving PKCE is
// actually enforced end to end, not just recorded. The signature also has no
// verifier parameter at all: a caller cannot supply one even if it wanted
// to, so this is the only source it could have come from.
func TestCompleteGoogleConnectPKCESendsTheStoredVerifier(t *testing.T) {
	tokenSrv, forms := recordingTokenServer(t)
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig(tokenSrv.URL)
	userID := uuid.New()

	verifier := oauth2.GenerateVerifier()
	authURL, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, verifier, userID, "")
	if err != nil {
		t.Fatalf("BeginGoogleConnectPKCE() = %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	state := u.Query().Get("state")

	// The profile fetch after a successful exchange hits the real Google
	// userinfo endpoint, which this test cannot and must not reach (Rules.md
	// §4: no test depends on the network). So this stops at
	// skerr.ErrValidation from fetchGoogleProfile — the assertion that
	// matters, that the token endpoint received the correct verifier, has
	// already happened by then.
	_, _, err = svc.CompleteGoogleConnectPKCE(context.Background(), cfg, state, "auth-code")
	if err == nil {
		t.Fatal("CompleteGoogleConnectPKCE() succeeded; expected the (unreachable in test) profile fetch to fail")
	}

	if len(*forms) != 1 {
		t.Fatalf("token requests received = %d, want 1", len(*forms))
	}
	if got := (*forms)[0].Get("code_verifier"); got != verifier {
		t.Errorf("code_verifier sent = %q, want %q", got, verifier)
	}
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// missingClientSecretTokenServer reproduces exactly what Google's real token
// endpoint returns when no client_secret is sent — reproduced live
// 2026-08-01, not invented from the RFC alone. It was originally read as
// proof the client was the wrong type; it is not. Google returns this for a
// genuine Desktop app client too, because it requires the secret from those
// as well.
func missingClientSecretTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_request",
			"error_description": "client_secret is missing.",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The secret Google requires must actually reach the token endpoint.
// Regression guard for the original bug: DesktopGoogleOAuthConfig set no
// ClientSecret at all, so every desktop exchange failed with
// `invalid_request: client_secret is missing` — and the diagnostic then blamed
// the user's Console setup for it.
func TestCompleteGoogleConnectPKCESendsTheClientSecret(t *testing.T) {
	tokenSrv, forms := recordingTokenServer(t)
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig(tokenSrv.URL)

	verifier := oauth2.GenerateVerifier()
	authURL, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, verifier, uuid.New(), "")
	if err != nil {
		t.Fatalf("BeginGoogleConnectPKCE() = %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}

	// Stops at the (unreachable in test) profile fetch; the token request has
	// already been made and recorded by then.
	_, _, _ = svc.CompleteGoogleConnectPKCE(context.Background(), cfg, u.Query().Get("state"), "auth-code")

	if len(*forms) != 1 {
		t.Fatalf("token requests received = %d, want 1", len(*forms))
	}
	got := (*forms)[0]
	if got.Get("client_secret") != cfg.ClientSecret {
		t.Errorf("client_secret sent = %q, want %q", got.Get("client_secret"), cfg.ClientSecret)
	}
	// Both, not either: Google wants the secret, and PKCE is what actually
	// secures a client whose secret ships inside the binary.
	if got.Get("code_verifier") != verifier {
		t.Errorf("code_verifier sent = %q, want %q", got.Get("code_verifier"), verifier)
	}
}

// When the secret really is absent, the message must point at our own unset
// variable. The message it replaced told the user to recreate their Google
// client as a "Desktop app" — advice that was wrong even when the client was
// already exactly that, which is the bug this whole change fixes.
func TestMissingClientSecretNamesTheUnsetVariableNotTheClientType(t *testing.T) {
	tokenSrv := missingClientSecretTokenServer(t)
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig(tokenSrv.URL)
	cfg.ClientSecret = ""

	verifier := oauth2.GenerateVerifier()
	authURL, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, verifier, uuid.New(), "")
	if err != nil {
		t.Fatalf("BeginGoogleConnectPKCE() = %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}

	_, _, err = svc.CompleteGoogleConnectPKCE(context.Background(), cfg, u.Query().Get("state"), "auth-code")
	if err == nil {
		t.Fatal("CompleteGoogleConnectPKCE() succeeded; expected the exchange to fail")
	}
	var pub *skerr.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("error = %v, want a *skerr.PublicError", err)
	}
	if !strings.Contains(pub.Message, "SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET") {
		t.Errorf("message = %q, want it to name the unset variable", pub.Message)
	}
	if !strings.Contains(pub.Message, cfg.ClientID) {
		t.Errorf("message = %q, want it to name the client id %q", pub.Message, cfg.ClientID)
	}
	// The old advice. Desktop app clients do have secrets, so telling anyone
	// to recreate their client as one is a dead end.
	if strings.Contains(pub.Message, "Desktop app") {
		t.Errorf("message = %q still blames the client type", pub.Message)
	}
}

// A configured secret that Google rejects for some other reason must not
// surface in anything the user sees. It is low-value (RFC 8252 §8.5) but
// there is no reason to echo it into a UI or a support paste.
func TestExchangeFailureNeverEchoesTheClientSecret(t *testing.T) {
	tokenSrv := missingClientSecretTokenServer(t)
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig(tokenSrv.URL)

	verifier := oauth2.GenerateVerifier()
	authURL, err := svc.BeginGoogleConnectPKCE(context.Background(), cfg, verifier, uuid.New(), "")
	if err != nil {
		t.Fatalf("BeginGoogleConnectPKCE() = %v", err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}

	_, _, err = svc.CompleteGoogleConnectPKCE(context.Background(), cfg, u.Query().Get("state"), "auth-code")
	if err == nil {
		t.Fatal("CompleteGoogleConnectPKCE() succeeded; expected the exchange to fail")
	}
	if strings.Contains(err.Error(), cfg.ClientSecret) {
		t.Errorf("error %q leaks the client secret", err.Error())
	}
}
