// Package app wires the Skein server — config, database, every domain
// service, the HTTP handler, and the background workers — independent of how
// the process is run. cmd/skein and cmd/skein-desktop both call Build; they
// differ only in how they open a listener and how they decide to shut down.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/config"
	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/db"
	"github.com/mridul60214/skein/internal/files"
	"github.com/mridul60214/skein/internal/httpapi"
	"github.com/mridul60214/skein/internal/httpapi/handlers"
	"github.com/mridul60214/skein/internal/logging"
	"github.com/mridul60214/skein/internal/router"
	"github.com/mridul60214/skein/internal/worker"
)

// Option customises Build. The only caller today is cmd/skein-desktop,
// wiring in its desktop OAuth connector — internal/app itself stays ignorant
// of what a "desktop connector" is beyond the interface handlers.Accounts
// already declares for it.
type Option func(*options)

type options struct {
	// desktopConnect is a factory rather than a built value: the connector
	// needs the *accounts.Service and *slog.Logger Build constructs
	// internally, which do not exist yet at the point a caller supplies
	// options, so Build calls this once they do.
	desktopConnect func(*accounts.Service, *slog.Logger) handlers.DesktopConnector
}

// WithDesktopConnect makes the accounts handler run the desktop OAuth flow
// (system browser, loopback listener, PKCE) instead of the web flow, and
// skips mounting the server-hosted OAuth callback route. Only
// cmd/skein-desktop passes this.
func WithDesktopConnect(newConnector func(*accounts.Service, *slog.Logger) handlers.DesktopConnector) Option {
	return func(o *options) { o.desktopConnect = newConnector }
}

// App is a fully wired Skein server bound to a listener. The caller owns
// process lifecycle — signals for the headless build, window-close for the
// desktop build — and drives it through Serve and Shutdown.
type App struct {
	Config *config.Config
	Logger *slog.Logger

	pool     *db.Pool
	listener net.Listener
	httpSrv  *http.Server
	workers  *worker.Runner
}

// Build loads configuration, migrates, and wires every service, exactly as
// cmd/skein's main did before this package existed. It does not open a
// listener or start serving — that is Addr/Serve's job, so the caller
// chooses the bind address (a fixed one for the headless server, 127.0.0.1:0
// for the desktop build) before anything is listening.
func Build(ctx context.Context, opts ...Option) (*App, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	lg := logging.New(cfg.LogLevel, cfg.LogJSON)
	slog.SetDefault(lg)
	lg.Info("starting skein", slog.String("env", cfg.Env), slog.String("addr", cfg.Addr))

	if !cfg.EncryptionEnabled {
		lg.Warn("encryption at rest is DISABLED; file content will be stored in plaintext")
	}

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	lg.Info("migrations applied")

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	masterKey, err := cfg.MasterKey()
	if err != nil {
		pool.Close()
		return nil, err
	}
	keyring, err := skcrypto.NewKeyring(masterKey)
	if err != nil {
		pool.Close()
		return nil, err
	}
	lg.Info("keyring ready", slog.String("key_id", keyring.KeyIDString()))

	authSvc := auth.NewService(
		auth.NewPGStore(pool),
		auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL),
		cfg.RefreshTokenTTL,
		lg.With(slog.String("component", "auth")),
	)

	var oauthCfg *oauth2.Config
	if cfg.GoogleConfigured() {
		oauthCfg = accounts.GoogleOAuthConfig(
			cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
	} else {
		lg.Warn("google oauth is not configured; drives cannot be connected",
			slog.String("fix", "set SKEIN_GOOGLE_CLIENT_ID, _SECRET and _REDIRECT_URL"))
	}

	accountsSvc := accounts.NewService(
		accounts.NewPGStore(pool),
		keyring,
		oauthCfg,
		lg.With(slog.String("component", "accounts")),
	)

	// No local fallback backend in a normal deployment: every shard belongs
	// to a connected drive, and silently writing to the server's own disk
	// when none is connected would be a surprise, not a convenience.
	resolver := accounts.NewResolver(accountsSvc, nil)

	// Capacity is claimed through the atomic conditional UPDATE in
	// Architecture.md §5, never through a Go map. The reserver also owns the
	// janitor that reclaims what a crashed upload left behind.
	reserver := router.NewReserver(
		router.NewPGStore(pool),
		lg.With(slog.String("component", "router")),
	)

	// storedSize has to account for ciphertext expansion, or a reservation
	// covers the plaintext and the drive runs out at the final frame.
	storedSize := func(plain int64) int64 { return plain }
	if cfg.EncryptionEnabled {
		storedSize = skcrypto.StreamOverhead
	}

	planner := router.NewPlanner(
		reserver,
		router.Policy(cfg.RoutingPolicy),
		cfg.ShardSizeBytes,
		storedSize,
	)
	lg.Info("shard routing configured",
		slog.String("policy", cfg.RoutingPolicy),
		slog.Int64("shard_size_bytes", cfg.ShardSizeBytes),
		slog.Int64("frames_per_shard", cfg.ShardSizeBytes/skcrypto.FrameSize))

	filesSvc := files.NewService(
		files.NewPGStore(pool),
		files.NewStripingPlanner(planner, reserver),
		resolver,
		keyring,
		files.Config{
			Encrypt:        cfg.EncryptionEnabled,
			MaxUploadBytes: cfg.MaxUploadBytes,
		},
		lg.With(slog.String("component", "files")),
	)

	var desktopConnect handlers.DesktopConnector
	if o.desktopConnect != nil {
		desktopConnect = o.desktopConnect(accountsSvc, lg.With(slog.String("component", "desktopoauth")))
	}

	srv, err := httpapi.New(httpapi.Deps{
		Config:         cfg,
		Logger:         lg,
		Health:         pool,
		Auth:           authSvc,
		Accounts:       accountsSvc,
		Files:          filesSvc,
		Keyring:        keyring,
		DesktopConnect: desktopConnect,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("build server: %w", err)
	}

	// Background loops. Quota sync is never on the upload path: uploads read
	// the cached figure and rely on the atomic reservation to stay correct.
	workers := worker.New(lg.With(slog.String("component", "worker")),
		worker.Job{
			Name:       "quota-sync",
			Every:      cfg.QuotaSyncEvery,
			RunAtStart: true,
			Timeout:    5 * time.Minute,
			Run:        accountsSvc.SyncAll,
		},
		worker.Job{
			Name:    "purge-oauth-states",
			Every:   15 * time.Minute,
			Timeout: time.Minute,
			Run: func(ctx context.Context) error {
				_, perr := accountsSvc.PurgeExpiredOAuthStates(ctx)
				return perr
			},
		},
		// The reclaim janitor. Without it, a killed process strands its
		// reservation forever and the pool shrinks every time something
		// goes wrong.
		worker.Job{
			Name:       "reclaim-reservations",
			Every:      cfg.ReclaimEvery,
			RunAtStart: true,
			Timeout:    time.Minute,
			Run:        reserver.ReclaimExpired,
		},
		worker.Job{
			Name:    "purge-sessions",
			Every:   time.Hour,
			Timeout: time.Minute,
			Run: func(ctx context.Context) error {
				_, perr := authSvc.PurgeExpiredSessions(ctx)
				return perr
			},
		},
	)

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// No WriteTimeout: responses stream multi-gigabyte file bodies and
		// a wall-clock write deadline would truncate them. Idle and header
		// timeouts still bound a slowloris.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return &App{
		Config:  cfg,
		Logger:  lg,
		pool:    pool,
		httpSrv: httpSrv,
		workers: workers,
	}, nil
}

// Listen opens the configured TCP listener. The headless server passes
// through cfg.Addr; the desktop build overrides it with "127.0.0.1:0" so the
// OS assigns a free port, then reads it back from Addr().
func (a *App) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	a.listener = ln
	return nil
}

// Addr returns the address Listen bound to, including the OS-assigned port
// when the caller asked for port 0. Empty until Listen has been called.
func (a *App) Addr() string {
	if a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

// Serve starts the background workers and blocks serving HTTP until the
// listener closes or Shutdown is called. It returns nil on a clean shutdown
// (http.ErrServerClosed) and the underlying error otherwise. Listen must be
// called first.
func (a *App) Serve(ctx context.Context) error {
	if a.listener == nil {
		return fmt.Errorf("serve: Listen was not called")
	}
	a.workers.Start(ctx)
	a.Logger.Info("listening", slog.String("addr", a.Addr()))
	if err := a.httpSrv.Serve(a.listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Shutdown drains in-flight requests within the given context, stops the
// background workers, and releases the database pool. Safe to call once,
// after Serve has returned or from another goroutine to trigger the return.
func (a *App) Shutdown(ctx context.Context) error {
	err := a.httpSrv.Shutdown(ctx)

	// Background loops stop when their context is cancelled by the caller;
	// wait for them so the process does not exit mid-write. This blocks on
	// the caller's ctx being done, not on Shutdown's — Wait has no deadline
	// of its own, so a caller that never cancels its worker context would
	// hang here forever. Both cmd/skein and cmd/skein-desktop cancel the
	// context Serve/Start ran under before calling Shutdown.
	a.workers.Wait()

	a.pool.Close()

	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
