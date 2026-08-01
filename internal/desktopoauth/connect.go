package desktopoauth

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/skerr"
)

// Connector runs the whole desktop OAuth round trip in one call: open a
// loopback listener, build the auth URL with a fresh PKCE verifier and the
// listener's own redirect URL, launch the system browser, wait for the
// callback, and complete the exchange. There is no server-hosted callback
// route on this path — the loopback listener is that route, for the
// duration of one attempt.
//
// Per RFC 8252, the authorisation request goes to the system's default
// browser, never the embedded webview: Google's own OAuth policies
// discourage embedded webviews for the consent screen, and only a real
// browser shows the user the address bar that makes the request legible.
type Connector struct {
	svc      *accounts.Service
	log      *slog.Logger
	clientID func() string
}

// NewConnector builds a connector. clientID is resolved at connect time, not
// at construction, so a client id set later (env var, or a future "use my
// own Google client" setting) takes effect without restarting.
func NewConnector(svc *accounts.Service, log *slog.Logger, clientID func() string) *Connector {
	return &Connector{svc: svc, log: log, clientID: clientID}
}

// Connect runs one attempt end to end and returns once the drive is linked
// or the attempt fails. ctx bounds the whole attempt, including the time
// spent waiting on the user's browser; cancelling it (e.g. the HTTP request
// that called this was itself cancelled) releases the loopback listener and
// gives up cleanly rather than leaking it.
func (c *Connector) Connect(ctx context.Context, userID uuid.UUID) (accounts.Account, error) {
	clientID := c.clientID()
	if clientID == "" {
		return accounts.Account{}, skerr.Public(skerr.ErrNotImplemented,
			"No Google client is configured for this app. Set SKEIN_GOOGLE_DESKTOP_CLIENT_ID and restart.")
	}

	listener, err := OpenLoopbackListener()
	if err != nil {
		return accounts.Account{}, fmt.Errorf("open loopback listener: %w", err)
	}
	// Await also closes the listener before returning, on every path. This
	// Close is only reached if Connect returns before calling Await at all
	// (the browser-launch failure below) — a second call is a safe no-op.
	// Close deliberately does not take ctx (see its own doc comment).
	defer listener.Close() //nolint:contextcheck

	cfg := accounts.DesktopGoogleOAuthConfig(clientID, listener.RedirectURL())
	verifier := oauth2.GenerateVerifier()

	authURL, err := c.svc.BeginGoogleConnectPKCE(ctx, cfg, verifier, userID, "")
	if err != nil {
		return accounts.Account{}, err
	}

	if err := browser.OpenURL(authURL); err != nil {
		c.log.WarnContext(ctx, "could not launch system browser", slog.String("error", err.Error()))
		return accounts.Account{}, skerr.Public(skerr.ErrUnavailable,
			"Could not open your browser. Copy this URL manually: %s", authURL)
	}

	result, err := listener.Await(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "desktop oauth attempt timed out or was cancelled",
			slog.String("user_id", userID.String()), slog.String("error", err.Error()))
		// Not ErrUnauthorized: that sentinel means "your session is invalid"
		// everywhere else in the API, and the frontend's generic 401 handler
		// treats it as exactly that — it clears the whole app session and,
		// worse, retries the request once against a freshly refreshed token
		// (api.ts's one-retry-after-refresh rule). For every other 401 that
		// retry is correct and harmless. Here it is not: a second retry
		// means a second loopback listener and a second browser tab, and
		// clearing the session logs the user out of the app entirely over
		// an OAuth attempt that timed out, which has nothing to do with
		// whether their Skein login is still valid. Reproduced 2026-08-01 —
		// this mapping is the actual cause of both symptoms.
		return accounts.Account{}, skerr.Public(skerr.ErrValidation,
			"That took too long. Try connecting again.")
	}
	if result.Err != "" {
		c.log.WarnContext(ctx, "desktop oauth cancelled or refused",
			slog.String("user_id", userID.String()), slog.String("provider_error", result.Err))
		return accounts.Account{}, skerr.Public(skerr.ErrValidation, "Connection cancelled.")
	}

	acct, _, err := c.svc.CompleteGoogleConnectPKCE(ctx, cfg, result.State, result.Code)
	return acct, err
}
