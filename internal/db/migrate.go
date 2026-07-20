package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrationsFS carries the goose SQL into the binary so a deployment is one
// file, per the third product principle.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every pending migration. It runs on boot, before the
// listener opens, so a process that is serving is a process that is migrated.
func Migrate(ctx context.Context, url string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer func() {
		if cerr := sqlDB.Close(); cerr != nil {
			slog.Warn("close migration connection", slog.String("error", cerr.Error()))
		}
	}()

	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrationStatus returns the current and highest known migration versions.
func MigrationStatus(ctx context.Context, sqlDB *sql.DB) (current int64, err error) {
	current, err = goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return 0, fmt.Errorf("read migration version: %w", err)
	}
	return current, nil
}
