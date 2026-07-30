// Command skein runs the Skein server: one binary, migrations and frontend
// included. It validates configuration, migrates, listens, and drains.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/oauth2"

	"github.com/mridul60214/skein/internal/accounts"
	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/config"
	skcrypto "github.com/mridul60214/skein/internal/crypto"
	"github.com/mridul60214/skein/internal/db"
	"github.com/mridul60214/skein/internal/files"
	"github.com/mridul60214/skein/internal/httpapi"
	"github.com/mridul60214/skein/internal/logging"
	"github.com/mridul60214/skein/internal/router"
	"github.com/mridul60214/skein/internal/worker"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	// Subcommands are a flat switch rather than a flag package: there are
	// two of them, and the default is the one everybody runs.
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "serve":
		err = run()
	case "migrate-folders":
		err = runMigrateFolders()
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "skein: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		// The logger may not exist yet if config failed, so this one
		// write goes to stderr directly.
		fmt.Fprintf(os.Stderr, "skein: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `skein — your free cloud accounts, pooled, striped and encrypted

  skein [serve]        run the server (default)
  skein migrate-folders  move shards left at drive root into the Skein folder

Configuration comes from the environment; see .env.example.
`)
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	lg := logging.New(cfg.LogLevel, cfg.LogJSON)
	slog.SetDefault(lg)
	lg.Info("starting skein",
		slog.String("version", version),
		slog.String("env", cfg.Env),
		slog.String("addr", cfg.Addr))

	if !cfg.EncryptionEnabled {
		lg.Warn("encryption at rest is DISABLED; file content will be stored in plaintext")
	}

	// Signal context first, so a Ctrl-C during migration is honoured.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err = db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	lg.Info("migrations applied")

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	masterKey, err := cfg.MasterKey()
	if err != nil {
		return err
	}
	keyring, err := skcrypto.NewKeyring(masterKey)
	if err != nil {
		return err
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

	srv, err := httpapi.New(httpapi.Deps{
		Config:   cfg,
		Logger:   lg,
		Health:   pool,
		Auth:     authSvc,
		Accounts: accountsSvc,
		Files:    filesSvc,
		Keyring:  keyring,
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
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
	workers.Start(ctx)

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
		// No WriteTimeout: responses stream multi-gigabyte file bodies
		// and a wall-clock write deadline would truncate them. Idle and
		// header timeouts still bound a slowloris.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		lg.Info("listening", slog.String("addr", cfg.Addr))
		if lerr := httpSrv.ListenAndServe(); lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen: %w", lerr)
			return
		}
		errCh <- nil
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-ctx.Done():
		stop() // restore default handling: a second signal kills immediately
		lg.Info("shutdown signal received; draining",
			slog.Duration("timeout", cfg.ShutdownTimeout))
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err = httpSrv.Shutdown(drainCtx); err != nil {
		lg.Error("drain did not finish cleanly", slog.String("error", err.Error()))
		return fmt.Errorf("shutdown: %w", err)
	}

	// Background loops stopped when ctx was cancelled; wait for them so the
	// process does not exit mid-write.
	workers.Wait()

	lg.Info("drained cleanly; exiting")
	return nil
}
