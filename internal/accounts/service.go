package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/skerr"
	"github.com/mridul249/Skein/internal/storage"
	"github.com/mridul249/Skein/internal/storage/gdrive"
)

const (
	// OAuthStateTTL bounds how long an authorisation may sit half-finished.
	// Ten minutes is long enough to pick an account and short enough that a
	// state captured from a browser history is useless.
	OAuthStateTTL = 10 * time.Minute

	// syncConcurrency caps parallel quota refreshes. Rules.md §2.12: bounded
	// parallelism, never one goroutine per row.
	syncConcurrency = 4

	// providerTimeout bounds a single provider metadata call.
	providerTimeout = 30 * time.Second
)

// ErrProviderNotConfigured reports a Drive connect attempt on a deployment
// with no Google credentials set.
var ErrProviderNotConfigured = errors.New("accounts: google oauth is not configured")

// Service links accounts and keeps their capacity fresh.
type Service struct {
	store   Store
	keyring *skcrypto.Keyring
	oauth   *oauth2.Config
	log     *slog.Logger

	// flight collapses concurrent quota refreshes for one account into a
	// single provider call. Ten uploads starting at once must not become ten
	// About requests.
	flight singleflight.Group

	// mu guards invalidators, which are registered during wiring but fired
	// from request goroutines, and the desktop OAuth credentials below.
	mu           sync.RWMutex
	invalidators []func(accountID uuid.UUID)

	// desktopClientID/Secret are the RFC 8252 installed-app credentials, set
	// by the desktop binary via SetDesktopOAuth.
	//
	// They exist because REFRESH and EXCHANGE take different paths. The
	// exchange builds a per-attempt config in desktopoauth.Connector and
	// passes it in as a parameter, so 28ba3ef's fix reached it. Refresh goes
	// through refreshConfig() below, which until 2026-08-05 could only see
	// the WEB config built from SKEIN_GOOGLE_CLIENT_ID/_SECRET/_REDIRECT_URL
	// — variables a desktop install never sets. The result was a nil config
	// on desktop and every Drive operation failing with
	// oauth2: "unauthorized_client".
	desktopClientID     string
	desktopClientSecret string

	now func() time.Time
}

// NewService wires the accounts service. oauthCfg may be nil, in which case
// connecting a Drive account returns a clear error instead of a panic.
func NewService(store Store, keyring *skcrypto.Keyring, oauthCfg *oauth2.Config, log *slog.Logger) *Service {
	return &Service{store: store, keyring: keyring, oauth: oauthCfg, log: log, now: time.Now}
}

// GoogleOAuthConfig builds the OAuth config for the drive.file scope.
func GoogleOAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{gdrive.Scope, "openid", "email"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/auth",
			TokenURL:  "https://oauth2.googleapis.com/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// DesktopGoogleOAuthConfig builds the OAuth config for a desktop (RFC 8252
// installed app) client: PKCE mandatory, and a redirect URL that is the
// caller's ephemeral loopback listener rather than a server-hosted callback
// route.
//
// clientSecret is required despite this being a public client, which reads
// like a contradiction and is worth stating plainly. Google's Auth Platform
// issues a secret for Desktop app clients and its token endpoint rejects the
// exchange without one — verified 2026-08-01 against a real Desktop app
// client (Console showed "Client ID for Desktop", a secret, and no
// authorised redirect URIs) whose exchange failed with
// `invalid_request: client_secret is missing`.
//
// That secret is not confidential and must not be treated as though it were.
// RFC 8252 §8.5 says an installed app cannot keep one, and Google documents
// it as such: it ships inside a binary any user can unpack. Security here
// rests on PKCE — which binds the code to the one client instance that
// requested it — and on the loopback redirect, never on the secret being
// unknown. Do not add it to the keyring or redact it from configuration on
// the theory that it protects anything; it does not.
func DesktopGoogleOAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{gdrive.Scope, "openid", "email"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/auth",
			TokenURL:  "https://oauth2.googleapis.com/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// SetDesktopOAuth supplies the installed-app credentials used to refresh
// tokens on the desktop build. Safe to call before serving starts; the desktop
// binary calls it during wiring.
//
// Kept separate from the web config rather than overwriting it: the two client
// types have different redirect requirements, and a desktop process that also
// had web credentials must not start refreshing desktop tokens against the web
// client.
func (s *Service) SetDesktopOAuth(clientID, clientSecret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desktopClientID = clientID
	s.desktopClientSecret = clientSecret
}

// refreshConfig returns the OAuth config used to refresh stored tokens.
//
// Desktop credentials win when present. A desktop install sets only
// SKEIN_GOOGLE_DESKTOP_CLIENT_ID/_SECRET, so without this the service is
// constructed with a nil config and refresh fails before it reaches Google.
//
// The redirect URL is deliberately empty: RFC 6749 §6 does not include
// redirect_uri in a refresh_token grant, and the loopback port that served the
// original exchange is long gone by then.
func (s *Service) refreshConfig() *oauth2.Config {
	s.mu.RLock()
	id, secret := s.desktopClientID, s.desktopClientSecret
	s.mu.RUnlock()

	if id != "" && secret != "" {
		return DesktopGoogleOAuthConfig(id, secret, "")
	}
	return s.oauth
}

// BeginGoogleConnect returns the URL to send the browser to.
//
// The state parameter is 32 random bytes. Only its SHA-256 is stored, with an
// expiry, and consuming it deletes the row — so a state is single use, cannot
// be replayed out of a browser history, and a database dump does not contain
// anything an attacker can present.
func (s *Service) BeginGoogleConnect(ctx context.Context, userID uuid.UUID, redirectTo string) (string, error) {
	if s.oauth == nil || s.oauth.ClientID == "" {
		return "", skerr.Public(skerr.ErrNotImplemented,
			"Google is not configured on this server. Set SKEIN_GOOGLE_CLIENT_ID and restart.")
	}
	return s.beginConnect(ctx, s.oauth, userID, redirectTo, "")
}

// BeginGoogleConnectPKCE is BeginGoogleConnect for a public client (RFC 8252):
// PKCE mandatory, and a caller-supplied config because the
// desktop build's redirect URL is a fresh loopback port picked per attempt
// and cannot be the single config wired at startup. verifier is generated by
// the caller with oauth2.GenerateVerifier and must be unique per attempt —
// asserted by TestPKCEVerifierIsNeverReusedAcrossAttempts.
func (s *Service) BeginGoogleConnectPKCE(ctx context.Context, cfg *oauth2.Config, verifier string, userID uuid.UUID, redirectTo string) (string, error) {
	if cfg == nil || cfg.ClientID == "" {
		return "", skerr.Public(skerr.ErrNotImplemented, "No Google client is configured for this connection.")
	}
	if verifier == "" {
		return "", fmt.Errorf("begin google connect pkce: empty verifier")
	}
	return s.beginConnect(ctx, cfg, userID, redirectTo, verifier)
}

func (s *Service) beginConnect(ctx context.Context, cfg *oauth2.Config, userID uuid.UUID, redirectTo, verifier string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(state))

	err := s.store.CreateOAuthState(ctx, sum[:], PendingOAuth{
		UserID:       userID,
		Kind:         storage.KindGoogleDrive,
		RedirectTo:   redirectTo,
		PKCEVerifier: verifier,
	}, s.now().Add(OAuthStateTTL))
	if err != nil {
		return "", fmt.Errorf("store oauth state: %w", err)
	}

	opts := []oauth2.AuthCodeOption{
		// AccessTypeOffline is what produces a refresh token at all.
		// ApprovalForce makes Google re-issue one even when the user has
		// consented before; without it, re-connecting an account yields a
		// grant with no refresh token and the link dies an hour later.
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	}
	if verifier != "" {
		opts = append(opts, oauth2.S256ChallengeOption(verifier))
	}
	return cfg.AuthCodeURL(state, opts...), nil
}

// CompleteGoogleConnect exchanges the authorisation code and links the account.
//
// Rules.md §2.4: linking is keyed on the provider's own account id, never on
// the email address. An address can be re-registered by someone else, and
// merging on email alone is account takeover. Nothing here consults the email
// for identity; it is stored as a label only.
func (s *Service) CompleteGoogleConnect(ctx context.Context, state, code string) (Account, string, error) {
	if s.oauth == nil || s.oauth.ClientID == "" {
		return Account{}, "", ErrProviderNotConfigured
	}
	return s.completeConnect(ctx, s.oauth, state, code)
}

// CompleteGoogleConnectPKCE is CompleteGoogleConnect for a desktop attempt.
// cfg must be the same config BeginGoogleConnectPKCE used — in particular the
// same RedirectURL, since Google validates it matches the authorisation
// request. The PKCE verifier is never taken from the caller: it is read back
// from the state row CompleteGoogleConnect already consumes, so a caller
// cannot complete an exchange with a verifier it did not originate.
func (s *Service) CompleteGoogleConnectPKCE(ctx context.Context, cfg *oauth2.Config, state, code string) (Account, string, error) {
	if cfg == nil || cfg.ClientID == "" {
		return Account{}, "", ErrProviderNotConfigured
	}
	return s.completeConnect(ctx, cfg, state, code)
}

func (s *Service) completeConnect(ctx context.Context, cfg *oauth2.Config, state, code string) (Account, string, error) {
	if state == "" || code == "" {
		return Account{}, "", skerr.Public(skerr.ErrValidation, "That authorisation link is not valid.")
	}

	sum := sha256.Sum256([]byte(state))
	pending, err := s.store.ConsumeOAuthState(ctx, sum[:])
	if err != nil {
		if errors.Is(err, skerr.ErrNotFound) {
			// Unknown, expired or already used — all the same from here,
			// and all mean "start again". ErrValidation, not ErrUnauthorized:
			// on the desktop path this reaches an authenticated handler
			// (accounts.Connector.Connect), and the frontend treats any 401
			// as "your Skein session died" — it clears the session and
			// retries the whole request once, which for a connect attempt
			// means a second loopback listener and a second browser tab.
			// This failure is about the OAuth attempt, never about whether
			// the caller is still logged into Skein. Reproduced 2026-08-01.
			return Account{}, "", skerr.Public(skerr.ErrValidation,
				"That authorisation expired or was already used. Try connecting again.")
		}
		return Account{}, "", fmt.Errorf("consume oauth state: %w", err)
	}

	exchangeCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	var opts []oauth2.AuthCodeOption
	if pending.PKCEVerifier != "" {
		opts = append(opts, oauth2.VerifierOption(pending.PKCEVerifier))
	}
	token, err := cfg.Exchange(exchangeCtx, code, opts...)
	if err != nil {
		// The provider's full error response can contain the code; it is
		// never logged or returned whole. RFC 6749's 'error' parameter
		// (invalid_grant, redirect_uri_mismatch, ...) is a fixed enum with
		// no secret in it, so it is the one piece worth surfacing — without
		// it, "Google rejected that authorisation" was the entire diagnosis
		// available for every failure, secret-carrying or not.
		var rerr *oauth2.RetrieveError
		// client_id is not a secret (RFC 8252) — safe to log unconditionally,
		// and it is what actually distinguishes "wrong client type" from
		// "wrong client entirely" when more than one Google client exists.
		fields := []any{slog.String("user_id", pending.UserID.String()), slog.String("client_id", cfg.ClientID)}
		if errors.As(err, &rerr) && rerr.ErrorCode != "" {
			fields = append(fields, slog.String("oauth_error_code", rerr.ErrorCode))
		}
		s.log.WarnContext(ctx, "oauth code exchange failed", fields...)

		// A missing client_secret on a config that has none means our own
		// configuration is incomplete, not that the user's Google client is
		// the wrong type. An earlier version of this branch said the
		// opposite — it told the user to recreate their client as "Desktop
		// app" — which was wrong twice over: Google issues secrets for
		// Desktop app clients too (see DesktopGoogleOAuthConfig), so the
		// advice sent people to recreate a client that was already correct,
		// and the real fault was that the secret was never wired through.
		// Point at the variable that is actually unset instead.
		if cfg.ClientSecret == "" && errors.As(err, &rerr) && rerr.ErrorCode == "invalid_request" &&
			strings.Contains(strings.ToLower(rerr.ErrorDescription), "client_secret") {
			return Account{}, "", skerr.Public(skerr.ErrValidation,
				"This build has no client secret configured for Google sign-in, and client %s requires one. "+
					"Set SKEIN_GOOGLE_DESKTOP_CLIENT_SECRET to the secret shown for that client "+
					"in Google Cloud Console and try again.", cfg.ClientID)
		}

		return Account{}, "", skerr.Public(skerr.ErrValidation,
			"Google rejected that authorisation. Try connecting again.")
	}

	profile, err := s.fetchGoogleProfile(ctx, token)
	if err != nil {
		return Account{}, "", err
	}

	acct, err := s.linkGoogleAccount(ctx, pending.UserID, token, profile)
	if err != nil {
		return Account{}, "", err
	}

	// First capacity read happens now so the UI has a number immediately
	// rather than after the first ticker interval.
	if err := s.SyncOne(ctx, acct.ID); err != nil {
		s.log.WarnContext(ctx, "initial quota sync failed",
			slog.String("account_id", acct.ID.String()),
			slog.String("error", err.Error()))
	}

	return acct, pending.RedirectTo, nil
}

func (s *Service) linkGoogleAccount(ctx context.Context, userID uuid.UUID, token *oauth2.Token, p googleProfile) (Account, error) {
	accessEnc, refreshEnc, err := s.encryptTokens(userID, token)
	if err != nil {
		return Account{}, err
	}
	var expiry *time.Time
	if !token.Expiry.IsZero() {
		e := token.Expiry
		expiry = &e
	}

	existing, err := s.store.GetAccountByProviderID(ctx, userID, storage.KindGoogleDrive, p.Sub)
	switch {
	case err == nil:
		// Re-connecting an account this user already linked: refresh the
		// credentials in place.
		updated, uerr := s.store.UpdateAccountTokens(ctx, TokenUpdate{
			ID:              existing.ID,
			UserID:          userID,
			AccessTokenEnc:  accessEnc,
			RefreshTokenEnc: refreshEnc,
			TokenExpiresAt:  expiry,
			Email:           p.Email,
			DisplayName:     p.Name,
		})
		if uerr != nil {
			return Account{}, fmt.Errorf("update account tokens: %w", uerr)
		}
		return updated.Account, nil

	case errors.Is(err, skerr.ErrNotFound):
		ordinal, oerr := s.store.NextOrdinal(ctx, userID)
		if oerr != nil {
			return Account{}, fmt.Errorf("next ordinal: %w", oerr)
		}
		created, cerr := s.store.CreateAccount(ctx, NewAccount{
			ID:                uuid.New(),
			UserID:            userID,
			Kind:              storage.KindGoogleDrive,
			ProviderAccountID: p.Sub,
			Email:             p.Email,
			DisplayName:       p.Name,
			AccessTokenEnc:    accessEnc,
			RefreshTokenEnc:   refreshEnc,
			TokenExpiresAt:    expiry,
			Ordinal:           ordinal,
		})
		if cerr != nil {
			if errors.Is(cerr, skerr.ErrConflict) {
				return Account{}, skerr.Public(skerr.ErrConflict,
					"That Google account is already connected.")
			}
			return Account{}, fmt.Errorf("create account: %w", cerr)
		}
		return created.Account, nil

	default:
		return Account{}, fmt.Errorf("look up account: %w", err)
	}
}

// encryptTokens seals both tokens under a key derived from the master key and
// salted with the user id. Rules.md §2.9 and the reference project's
// sha256(passphrase): the salt means two users' tokens never share a key, and
// the envelope's version byte means the format can change later.
func (s *Service) encryptTokens(userID uuid.UUID, token *oauth2.Token) (access, refresh []byte, err error) {
	salt := userID[:]

	access, err = s.keyring.SealString(skcrypto.InfoToken, salt, token.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("seal access token: %w", err)
	}
	if token.RefreshToken != "" {
		refresh, err = s.keyring.SealString(skcrypto.InfoToken, salt, token.RefreshToken)
		if err != nil {
			return nil, nil, fmt.Errorf("seal refresh token: %w", err)
		}
	}
	return access, refresh, nil
}

// credentialsFor decrypts an account's stored tokens.
func (s *Service) credentialsFor(acct StoredAccount) (Credentials, error) {
	salt := acct.UserID[:]

	accessToken, err := s.keyring.OpenString(skcrypto.InfoToken, salt, acct.AccessTokenEnc)
	if err != nil {
		return Credentials{}, fmt.Errorf("open access token for account %s: %w", acct.ID, err)
	}
	var refreshToken string
	if len(acct.RefreshTokenEnc) > 0 {
		refreshToken, err = s.keyring.OpenString(skcrypto.InfoToken, salt, acct.RefreshTokenEnc)
		if err != nil {
			return Credentials{}, fmt.Errorf("open refresh token for account %s: %w", acct.ID, err)
		}
	}
	return Credentials{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    acct.TokenExpires,
	}, nil
}

// Backend returns a storage.Backend for an account, with a token source that
// refreshes and re-encrypts as needed.
func (s *Service) Backend(ctx context.Context, userID, accountID uuid.UUID) (storage.Backend, error) {
	acct, err := s.store.GetAccount(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	return s.backendFor(ctx, acct)
}

func (s *Service) backendFor(ctx context.Context, acct StoredAccount) (storage.Backend, error) {
	// A disconnected account keeps its row so shard links survive (see
	// Disconnect), which means "the row exists" no longer implies "the drive
	// is usable". Its credentials are wiped, so without this check the
	// failure would surface as an opaque token error from the provider
	// instead of a message the user can act on.
	if acct.Status == StatusDisabled {
		return nil, skerr.Public(skerr.ErrUnavailable,
			"That drive is disconnected. Reconnect it in Settings to reach these files.")
	}
	if acct.Kind != storage.KindGoogleDrive {
		return nil, fmt.Errorf("accounts: no backend for kind %q", acct.Kind)
	}
	oauthCfg := s.refreshConfig()
	if oauthCfg == nil {
		return nil, ErrProviderNotConfigured
	}

	creds, err := s.credentialsFor(acct)
	if err != nil {
		return nil, err
	}

	tok := &oauth2.Token{
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		TokenType:    "Bearer",
	}
	if creds.ExpiresAt != nil {
		tok.Expiry = *creds.ExpiresAt
	}

	// persistingSource writes a refreshed token back as ciphertext, so a
	// rotated refresh token is not lost when the process restarts.
	detached := context.WithoutCancel(ctx)
	src := &persistingSource{
		svc:    s,
		acct:   acct,
		ctx:    detached,
		source: oauthCfg.TokenSource(detached, tok),
	}

	client := oauth2.NewClient(context.WithoutCancel(ctx), src)
	// The transport must not impose its own deadline: uploads are bounded
	// by the request context and can legitimately run for hours.
	client.Timeout = 0
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}

	// Establish the app folder before handing out a backend, so every shard
	// this backend writes is parented correctly. A failure here is not fatal
	// to the upload: shards land at root as they did before, which is worse
	// but not broken, and the next attempt retries.
	folderID, err := s.ensureAppFolder(ctx, acct, client)
	if err != nil {
		s.log.WarnContext(ctx, "could not establish the provider app folder; "+
			"shards will land at drive root until this succeeds",
			slog.String("account_id", acct.ID.String()),
			slog.String("error", err.Error()))
		folderID = ""
	}

	return gdrive.New(client, folderID), nil
}

// SyncOne refreshes one account's capacity.
func (s *Service) SyncOne(ctx context.Context, accountID uuid.UUID) error {
	// singleflight keyed by account: concurrent callers share one provider
	// round trip. Rules.md §2.12 and Architecture.md §11.
	_, err, _ := s.flight.Do(accountID.String(), func() (any, error) {
		return nil, s.syncAccountByID(ctx, accountID)
	})
	return err
}

func (s *Service) syncAccountByID(ctx context.Context, accountID uuid.UUID) error {
	accts, err := s.store.ListAccountsForSync(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	for _, a := range accts {
		if a.ID == accountID {
			return s.syncAccount(ctx, a)
		}
	}
	return skerr.ErrNotFound
}

// SyncUserAccount refreshes one account the caller owns.
func (s *Service) SyncUserAccount(ctx context.Context, userID, accountID uuid.UUID) error {
	acct, err := s.store.GetAccount(ctx, userID, accountID)
	if err != nil {
		return err
	}
	_, err, _ = s.flight.Do(acct.ID.String(), func() (any, error) {
		return nil, s.syncAccount(ctx, acct)
	})
	return err
}

// SyncAll refreshes every active account. It is what the background ticker
// runs, and it is never on the upload hot path.
func (s *Service) SyncAll(ctx context.Context) error {
	accts, err := s.store.ListAccountsForSync(ctx)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(syncConcurrency)

	for _, a := range accts {
		g.Go(func() error {
			// One failing provider must not abort the others, so the
			// error is recorded against the account and swallowed here.
			if err := s.syncAccount(gctx, a); err != nil {
				s.log.WarnContext(gctx, "quota sync failed",
					slog.String("account_id", a.ID.String()),
					slog.String("kind", string(a.Kind)),
					slog.String("error", err.Error()))
			}
			return nil
		})
	}
	return g.Wait()
}

func (s *Service) syncAccount(ctx context.Context, acct StoredAccount) error {
	backend, err := s.backendFor(ctx, acct)
	if err != nil {
		return s.recordSyncFailure(ctx, acct, err)
	}

	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	quota, err := backend.Quota(ctx)
	if err != nil {
		return s.recordSyncFailure(ctx, acct, err)
	}

	if err := s.store.UpsertCapacity(ctx, acct.ID, quota.TotalBytes, quota.UsedBytes); err != nil {
		return fmt.Errorf("store capacity: %w", err)
	}
	if acct.Status != StatusActive {
		if err := s.store.SetAccountStatus(ctx, acct.ID, StatusActive, ""); err != nil {
			return fmt.Errorf("clear account status: %w", err)
		}
	}
	return nil
}

// recordSyncFailure marks the account so the UI can say what is wrong. A
// revoked grant becomes needs_reauth, which is actionable; anything else is
// recorded but leaves the account active, because a transient provider error
// should not require the user to re-authorise.
func (s *Service) recordSyncFailure(ctx context.Context, acct StoredAccount, cause error) error {
	msg := "Could not reach the provider."
	status := acct.Status

	// A rate limit is explicitly NOT a reason to re-authorise. Drive reports
	// it as 403, the same status as a revoked grant, and gdrive.apiError is
	// what tells the two apart; if that mapping regresses, a busy account
	// starts demanding re-consent. Handled before the ErrUnauthorized branch
	// so a future error that satisfies both cannot fall through to it.
	switch {
	case errors.Is(cause, storage.ErrRateLimited):
		msg = "Google is rate limiting this drive. It will retry on its own."

	// A misconfigured OAuth CLIENT is not the user's problem and reconnecting
	// cannot fix it. Marking the account needs_reauth here would send them
	// round a loop that can never succeed, so the account stays active and the
	// message names the real fault. Checked BEFORE the revoked-grant branch so
	// a config error can never fall through to it.
	case errors.Is(cause, ErrRefreshClientMisconfigured):
		msg = "Skein's Google client is misconfigured. This is a server " +
			"setting, not something reconnecting will fix."

	case errors.Is(cause, ErrRefreshGrantRevoked) ||
		errors.Is(cause, storage.ErrUnauthorized) || errors.Is(cause, skcrypto.ErrDecrypt):
		status = StatusNeedsReauth
		msg = "Google revoked access. Reconnect this drive."
	}

	if status != acct.Status {
		if err := s.store.SetAccountStatus(ctx, acct.ID, status, msg); err != nil {
			return fmt.Errorf("set account status: %w", err)
		}
	}
	if err := s.store.SetCapacityError(ctx, acct.ID, msg); err != nil {
		return fmt.Errorf("set capacity error: %w", err)
	}
	return fmt.Errorf("sync account %s: %w", acct.ID, cause)
}

// List returns the caller's connected accounts.
//
// Disabled rows are filtered out. Disconnect keeps them so shard links survive
// (see Disconnect), but to the user that drive is gone — listing it would show
// a disconnected drive as connected, and the row is an implementation detail
// they never asked to see.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Account, error) {
	stored, err := s.store.ListAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out := make([]Account, 0, len(stored))
	for _, a := range stored {
		if a.Status == StatusDisabled {
			continue
		}
		out = append(out, a.Account)
	}
	return out, nil
}

// PoolFor returns per-account and aggregate capacity for the caller.
//
// Disabled accounts stay in Accounts but contribute nothing to the totals, so
// the UI can show a drive and explain why it is not counting (QuotaBar.tsx
// filters them out of the bar itself). Unlike List, this deliberately does not
// hide them — asserted by TestDisabledAccountsAreExcludedFromThePool.
func (s *Service) PoolFor(ctx context.Context, userID uuid.UUID) (Pool, error) {
	caps, err := s.store.ListCapacity(ctx, userID)
	if err != nil {
		return Pool{}, fmt.Errorf("list capacity: %w", err)
	}

	pool := Pool{Accounts: caps}
	for _, c := range caps {
		if c.Account.Status == StatusDisabled {
			continue
		}
		pool.TotalBytes += c.TotalBytes
		pool.UsedBytes += c.UsedBytes
		pool.FreeBytes += c.FreeBytes()
	}
	return pool, nil
}

// Disconnect unlinks an account, marking it disabled rather than deleting the
// row (known issue #19).
//
// The objects it holds are deliberately left alone: Skein has no way to know
// whether the user wants the provider-side data gone, and deleting somebody's
// files as a side effect of unlinking is not a decision this code gets to make.
// Files that depended on the account become unreadable and report so.
//
// The row itself must survive for the same reason. file_shards.
// connected_account_id is ON DELETE SET NULL (00004_files.sql:64), so deleting
// it nulled the link on every shard the drive held — and because nothing else
// records which drive a shard lived on, that link was unrecoverable. Re-adding
// the same Google account inserted a fresh row with a new uuid and re-linked
// nothing, leaving those files permanently unreadable even though every byte
// was still sitting in the user's Drive. Keeping the id stable is what makes
// reconnecting restore access: linkGoogleAccount finds the existing row by
// provider account id and updates it in place, resetting status to 'active'.
//
// Credentials are cleared here. A disabled account must not keep usable
// tokens: the user asked for it to stop having access, and reconnecting
// supplies new ones anyway.
func (s *Service) Disconnect(ctx context.Context, userID, accountID uuid.UUID) error {
	// Scoped by userID so one user cannot disable another's account — the
	// delete this replaced got that from its own WHERE clause.
	acct, err := s.store.GetAccount(ctx, userID, accountID)
	if err != nil {
		return err
	}
	// Disconnecting an already-disconnected drive is ErrNotFound, as it was
	// when the row was deleted outright. The row surviving is an
	// implementation detail of keeping shard links intact; it must not turn
	// a repeat disconnect into a silent success.
	if acct.Status == StatusDisabled {
		return skerr.ErrNotFound
	}
	if err := s.store.ClearAccountTokens(ctx, accountID); err != nil {
		return fmt.Errorf("clear account tokens: %w", err)
	}
	if err := s.store.SetAccountStatus(ctx, accountID, StatusDisabled, ""); err != nil {
		return fmt.Errorf("disable account: %w", err)
	}
	s.invalidate(accountID)
	return nil
}

// OnAccountInvalidated registers a callback fired when an account's
// credentials stop being valid. Resolver uses it to drop its cached backend:
// that cache is checked before the account is ever re-read, so without this a
// disconnected drive keeps serving from a backend built on the old grant for
// the life of the process.
func (s *Service) OnAccountInvalidated(fn func(accountID uuid.UUID)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidators = append(s.invalidators, fn)
}

func (s *Service) invalidate(accountID uuid.UUID) {
	s.mu.RLock()
	fns := make([]func(uuid.UUID), len(s.invalidators))
	copy(fns, s.invalidators)
	s.mu.RUnlock()
	for _, fn := range fns {
		fn(accountID)
	}
}

// PurgeExpiredOAuthStates removes abandoned authorisations.
func (s *Service) PurgeExpiredOAuthStates(ctx context.Context) (int64, error) {
	n, err := s.store.DeleteExpiredOAuthStates(ctx)
	if err != nil {
		return 0, fmt.Errorf("purge oauth states: %w", err)
	}
	return n, nil
}

// persistingSource writes a refreshed OAuth token back to the database as
// ciphertext. Without it, every process restart would re-run the refresh, and
// a provider that rotates refresh tokens would eventually invalidate the one
// on disk.
type persistingSource struct {
	svc  *Service
	acct StoredAccount
	// ctx is detached from the request (context.WithoutCancel) because a
	// refresh triggered by a cancelled download still has to persist its
	// rotated token and record a revoked grant.
	ctx    context.Context
	source oauth2.TokenSource
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.source.Token()
	if err != nil {
		// Classify at the refresh boundary: this failure happens inside the
		// oauth2 library, before any Drive call, so gdrive.apiError never sees
		// it. Without this the account stays 'active' while every operation
		// fails, and the caller gets a bare 500.
		classified := classifyRefreshError(err)

		// Record the transition here rather than leaving it to the caller.
		// A refresh failure can surface from a download, a quota sync or an
		// app-folder probe, and only some of those run through
		// recordSyncFailure — so marking it at the one point every refresh
		// passes through is what makes the account state actually track
		// reality. Only a revoked grant transitions; a misconfigured client
		// deliberately does not (see recordSyncFailure).
		if errors.Is(classified, ErrRefreshGrantRevoked) {
			if serr := p.svc.store.SetAccountStatus(context.WithoutCancel(p.ctx),
				p.acct.ID, StatusNeedsReauth,
				"Google revoked access. Reconnect this drive."); serr != nil {
				p.svc.log.WarnContext(p.ctx, "could not mark account needs_reauth",
					slog.String("account_id", p.acct.ID.String()),
					slog.String("error", serr.Error()))
			}
		}
		if errors.Is(classified, ErrRefreshClientMisconfigured) {
			p.svc.log.ErrorContext(p.ctx, "the google oauth client is misconfigured; "+
				"drive token refresh cannot succeed until it is fixed",
				slog.String("account_id", p.acct.ID.String()),
				slog.String("fix", "set SKEIN_GOOGLE_DESKTOP_CLIENT_ID/_SECRET "+
					"on desktop, or SKEIN_GOOGLE_CLIENT_ID/_SECRET on the server"),
				slog.String("error", classified.Error()))
		}

		// The API shape: {code, message, account_id}, 409 for a dead grant so
		// the frontend does not read it as a lost Skein session.
		return nil, AsAPIError(fmt.Errorf("refresh provider token: %w", classified),
			p.acct.ID, p.acct.Email)
	}

	// Nothing changed; no write needed.
	if p.acct.TokenExpires != nil && tok.Expiry.Equal(*p.acct.TokenExpires) {
		return tok, nil
	}

	access, refresh, err := p.svc.encryptTokens(p.acct.UserID, tok)
	if err != nil {
		// The token is usable even if it cannot be persisted; the next
		// process would just refresh again.
		p.svc.log.Warn("could not seal refreshed token",
			slog.String("account_id", p.acct.ID.String()))
		return tok, nil
	}

	var expiry *time.Time
	if !tok.Expiry.IsZero() {
		e := tok.Expiry
		expiry = &e
	}

	// Detached context: this runs inside a transport round trip whose
	// context may be cancelled the moment the caller gives up, and losing
	// the rotated refresh token to a cancellation is worse than the write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), providerTimeout)
	defer cancel()

	updated, err := p.svc.store.UpdateAccountTokens(ctx, TokenUpdate{
		ID:              p.acct.ID,
		UserID:          p.acct.UserID,
		AccessTokenEnc:  access,
		RefreshTokenEnc: refresh,
		TokenExpiresAt:  expiry,
		Email:           p.acct.Email,
		DisplayName:     p.acct.DisplayName,
	})
	if err != nil {
		p.svc.log.Warn("could not persist refreshed token",
			slog.String("account_id", p.acct.ID.String()),
			slog.String("error", err.Error()))
		return tok, nil
	}
	p.acct = updated
	return tok, nil
}
