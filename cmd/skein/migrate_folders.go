package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mridul249/Skein/internal/accounts"
	"github.com/mridul249/Skein/internal/config"
	skcrypto "github.com/mridul249/Skein/internal/crypto"
	"github.com/mridul249/Skein/internal/db"
	"github.com/mridul249/Skein/internal/logging"
)

// runMigrateFolders moves shards that predate the app folder out of Drive root.
//
// Shards used to land at root, where they look like junk with names nobody
// recognises — and people delete junk. This is the one-shot that files them
// away. It is idempotent: a second run finds nothing at root and reports zero
// moved, so it is safe to re-run after a partial failure.
func runMigrateFolders() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	lg := logging.New(cfg.LogLevel, cfg.LogJSON)
	slog.SetDefault(lg)

	if !cfg.GoogleConfigured() {
		return fmt.Errorf("google oauth is not configured; " +
			"set SKEIN_GOOGLE_CLIENT_ID, _SECRET and _REDIRECT_URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
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

	svc := accounts.NewService(
		accounts.NewPGStore(pool),
		keyring,
		accounts.GoogleOAuthConfig(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL),
		lg.With(slog.String("component", "accounts")),
	)

	reports, err := svc.MigrateFolders(ctx)
	if err != nil {
		return err
	}

	var moved, failed, drives int
	for _, r := range reports {
		drives++
		if r.Err != nil {
			fmt.Printf("  %-28s error: %v\n", r.Email, r.Err)
			failed++
			continue
		}
		fmt.Printf("  %-28s folder %s  moved %d  failed %d\n",
			r.Email, shortID(r.FolderID), r.Moved, r.Failed)
		moved += r.Moved
		failed += r.Failed
	}

	fmt.Printf("\n%d drive(s), %d shard(s) moved, %d failure(s)\n", drives, moved, failed)
	if failed > 0 {
		// A non-zero exit so a script notices. Nothing was lost — the
		// shards that failed are still at root and a re-run retries them.
		return fmt.Errorf("%d object(s) could not be moved; re-run to retry", failed)
	}
	return nil
}

func shortID(id string) string {
	if id == "" {
		return "(none)"
	}
	if len(id) <= 12 {
		return id
	}
	return id[:6] + "…" + id[len(id)-4:]
}
