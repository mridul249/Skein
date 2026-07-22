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

	"github.com/mridul60214/skein/internal/auth"
	"github.com/mridul60214/skein/internal/config"
	"github.com/mridul60214/skein/internal/db"
	"github.com/mridul60214/skein/internal/httpapi"
	"github.com/mridul60214/skein/internal/logging"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so this one
		// write goes to stderr directly.
		fmt.Fprintf(os.Stderr, "skein: %v\n", err)
		os.Exit(1)
	}
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

	authSvc := auth.NewService(
		auth.NewPGStore(pool),
		auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL),
		cfg.RefreshTokenTTL,
		lg.With(slog.String("component", "auth")),
	)

	srv, err := httpapi.New(httpapi.Deps{
		Config: cfg,
		Logger: lg,
		Health: pool,
		Auth:   authSvc,
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

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
	lg.Info("drained cleanly; exiting")
	return nil
}
