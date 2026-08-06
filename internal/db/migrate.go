package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// migrationsFS carries the goose SQL into the binary so a deployment is one
// file, per the third product principle.
//
//go:embed migrations/*.sql
//go:embed migrations/sqlite/*.sql
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

// OpenSQLite opens the desktop database, creating its parent directory and
// applying the SQLite migrations. The returned *sql.DB is ready for every
// SQLite-backed store in the desktop build.
func OpenSQLite(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// ONE CONNECTION. SQLite permits exactly one writer, and this is the fix
	// for a live stall reported 2026-08-06: a 400 MB upload froze at 4.1 MB
	// while every storage account logged
	// "set storage account error: context deadline exceeded".
	//
	// database/sql opens connections without limit by default. That turns
	// SQLite's single write lock into N goroutines racing a busy_timeout: an
	// upload holds the lock across its shard inserts, the 30s quota sweep
	// fires into that window under a 3s deadline, and the sweep loses every
	// time. The user also saw a SMALL upload stall when started alongside a
	// large one — the same contention from the other side.
	//
	// Capping the pool makes database/sql QUEUE writers in Go instead. That is
	// fair (FIFO), bounded, and converts lock contention into a wait rather
	// than an error. Raising busy_timeout ALONE would not do it: the losers
	// still race, so the failure returns under enough load. The longer timeout
	// stays as a backstop for contention SQLite resolves internally, such as a
	// WAL checkpoint.
	//
	// The cost is real and accepted: reads serialise behind writes too. This
	// is a single-user desktop database, and a correct wait is worth more than
	// a concurrent failure. If read throughput ever matters, the answer is a
	// second read-only pool, not a larger writer pool.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	// No lifetime cap: recycling the single connection would drop the
	// per-connection PRAGMAs set via the DSN.
	sqlDB.SetConnMaxLifetime(0)

	if err := MigrateSQLite(ctx, sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return sqlDB, nil
}

// DesktopSQLitePath returns the default per-user database path for
// skein-desktop. SKEIN_SQLITE_PATH is an escape hatch for development and
// portable installs.
func DesktopSQLitePath() (string, error) {
	if v := os.Getenv("SKEIN_SQLITE_PATH"); v != "" {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(dir, "skein", "skein.db"), nil
}

// MigrateSQLite applies every pending desktop migration.
func MigrateSQLite(ctx context.Context, sqlDB *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set sqlite dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(ctx, sqlDB, "migrations/sqlite"); err != nil {
		return fmt.Errorf("apply sqlite migrations: %w", err)
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
