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
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/skerr"
	"github.com/mridul60214/skein/internal/storage"
	"github.com/mridul60214/skein/internal/storage/gdrive"
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
	if acct.Kind != storage.KindGoogleDrive {
		return nil, fmt.Errorf("accounts: no backend for kind %q", acct.Kind)
	}
	if s.oauth == nil {
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
	src := &persistingSource{
		svc:    s,
		acct:   acct,
		source: s.oauth.TokenSource(context.WithoutCancel(ctx), tok),
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

	if errors.Is(cause, storage.ErrUnauthorized) || errors.Is(cause, skcrypto.ErrDecrypt) {
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

// List returns the caller's accounts.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Account, error) {
	stored, err := s.store.ListAccounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out := make([]Account, 0, len(stored))
	for _, a := range stored {
		out = append(out, a.Account)
	}
	return out, nil
}

// PoolFor returns per-account and aggregate capacity for the caller.
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

// Disconnect removes an account.
//
// The objects it holds are deliberately left alone: Skein has no way to know
// whether the user wants the provider-side data gone, and deleting somebody's
// files as a side effect of unlinking is not a decision this code gets to make.
// Files that depended on the account become unreadable and report so.
func (s *Service) Disconnect(ctx context.Context, userID, accountID uuid.UUID) error {
	n, err := s.store.DeleteAccount(ctx, userID, accountID)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n == 0 {
		return skerr.ErrNotFound
	}
	return nil
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
	svc    *Service
	acct   StoredAccount
	source oauth2.TokenSource
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.source.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh provider token: %w", err)
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
