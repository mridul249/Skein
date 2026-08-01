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

// fakeTokenServer stands in for Google's token endpoint. It records every
// code_verifier it is presented and always issues a token — the tests below
// inspect what was sent rather than have the server itself gate on it.
func fakeTokenServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var verifiers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token request: %v", err)
		}
		verifiers = append(verifiers, r.Form.Get("code_verifier"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access-token",
			"refresh_token": "fake-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &verifiers
}

func testPKCEConfig(tokenURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    "desktop-client-id",
		RedirectURL: "http://127.0.0.1:0/callback",
		Scopes:      []string{"drive.file"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/auth",
			TokenURL:  tokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

func TestBeginGoogleConnectPKCERequiresNoClientSecret(t *testing.T) {
	svc, _, _ := newTestService(t, false)
	cfg := testPKCEConfig("https://example.invalid/token")
	if cfg.ClientSecret != "" {
		t.Fatal("test config carries a client secret; the point of this test is that it doesn't need one")
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
		t.Error("auth URL carries a client_secret; desktop clients must not send one")
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
	tokenSrv, verifiers := fakeTokenServer(t)
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

	if len(*verifiers) != 1 {
		t.Fatalf("token requests received = %d, want 1", len(*verifiers))
	}
	if (*verifiers)[0] != verifier {
		t.Errorf("code_verifier sent = %q, want %q", (*verifiers)[0], verifier)
	}
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// missingClientSecretTokenServer reproduces exactly what Google's real
// token endpoint returns when a Google Cloud OAuth client was registered as
// "Web application" instead of "Desktop app" — reproduced live 2026-08-01
// against a real misconfigured client, not invented from the RFC alone.
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

// The single most likely real-world mistake for anyone setting up their own
// desktop OAuth client (Phase7 Task 4.4 point 6, CONFIGURATION.md's
// SKEIN_GOOGLE_DESKTOP_CLIENT_ID): registering it as "Web application" in
// Google Cloud Console instead of "Desktop app". Web application clients
// require a client_secret at exchange regardless of PKCE; Desktop app
// clients are the only type Google exempts from that. The generic "Google
// rejected that authorisation" message gave no way to tell this apart from
// an expired code or a network blip — this asserts the specific,
// actionable message fires instead.
func TestCompleteGoogleConnectPKCENamesTheWrongClientTypeMistake(t *testing.T) {
	tokenSrv := missingClientSecretTokenServer(t)
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

	_, _, err = svc.CompleteGoogleConnectPKCE(context.Background(), cfg, state, "auth-code")
	if err == nil {
		t.Fatal("CompleteGoogleConnectPKCE() succeeded; expected the exchange to fail")
	}
	var pub *skerr.PublicError
	if !errors.As(err, &pub) {
		t.Fatalf("error = %v, want a *skerr.PublicError", err)
	}
	if !strings.Contains(pub.Message, "Desktop app") {
		t.Errorf("message = %q, want it to name the Desktop app client type mistake", pub.Message)
	}
}
