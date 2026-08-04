package db

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Dialect names which engine a dump came from. It goes in the filename because
// Postgres and SQLite report different schema versions — the numbering is
// independent by design, not drifting — and a bare "v4" or "v8" with no engine
// beside it is the kind of thing that gets "fixed".
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

// ErrBackupBusy reports a dump already running. Backups are serialised rather
// than queued: two concurrent pg_dumps against the same database is a way to
// turn a maintenance action into an outage.
var ErrBackupBusy = errors.New("db: a backup is already running")

// Dump writes a gzipped logical backup of the database to w.
//
// THE FAILURE MODE THIS IS BUILT AROUND: `pg_dump | gzip` under a shell reports
// the exit status of the LAST command, so a pg_dump that never connected still
// produced a valid, empty .gz that looked like a successful backup. That was a
// real bug (see 7b23f20 and internal/db/backup_test.go). Nothing here uses a
// shell, the subprocess's own exit status is checked explicitly via Wait, and
// the caller is expected not to have written a byte of response body until
// Dump returns nil.
type Dumper struct {
	// DatabaseURL is the Postgres connection string. Empty for SQLite.
	DatabaseURL string
	// SQLitePath is the database file. Empty for Postgres.
	SQLitePath string
	// Dialect selects the strategy.
	Dialect Dialect

	// running serialises dumps. Buffered to one.
	running chan struct{}
}

// NewDumper builds a Dumper for whichever engine the app is running on.
func NewDumper(dialect Dialect, databaseURL, sqlitePath string) *Dumper {
	return &Dumper{
		DatabaseURL: databaseURL,
		SQLitePath:  sqlitePath,
		Dialect:     dialect,
		running:     make(chan struct{}, 1),
	}
}

// Filename is what the dump should be served as. The dialect and schema
// version are both in the name so a restorer can tell at a glance which engine
// and which migration state a file came from.
func (d *Dumper) Filename(schemaVersion int64, now time.Time) string {
	return fmt.Sprintf("skein-backup-%s-%s-v%d.sql.gz",
		now.UTC().Format("20060102-150405"), d.Dialect, schemaVersion)
}

// Dump streams a gzipped dump to w. It returns an error without writing
// anything meaningful to w when the dump itself fails.
//
// The caller must not send response headers until this returns nil on a
// buffered writer, or must accept that a mid-stream failure truncates the
// response. See handlers: it buffers so a failure can still become a 500.
func (d *Dumper) Dump(ctx context.Context, w io.Writer) error {
	select {
	case d.running <- struct{}{}:
		defer func() { <-d.running }()
	default:
		return ErrBackupBusy
	}

	switch d.Dialect {
	case DialectPostgres:
		return d.dumpPostgres(ctx, w)
	case DialectSQLite:
		return d.dumpSQLite(ctx, w)
	default:
		return fmt.Errorf("db: unknown dialect %q", d.Dialect)
	}
}

// dumpPostgres runs pg_dump as a subprocess and gzips its stdout in-process.
//
// exec.Command with an argument list, never a shell: there is no pipeline
// whose exit status could mask pg_dump's, and no quoting for a connection
// string to escape from.
func (d *Dumper) dumpPostgres(ctx context.Context, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--no-owner", "--no-privileges", d.DatabaseURL)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pg_dump stdout: %w", err)
	}
	// stderr is captured rather than discarded: "database does not exist" is
	// the whole diagnosis, and losing it leaves an operator with "exit 1".
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if serr := cmd.Start(); serr != nil {
		return fmt.Errorf("start pg_dump: %w", serr)
	}

	gz := gzip.NewWriter(w)
	_, copyErr := io.Copy(gz, stdout)

	// Wait AFTER draining stdout, and check it explicitly. This is the line
	// the original bug did not have.
	waitErr := cmd.Wait()

	if waitErr != nil {
		// Do not close the gzip writer on failure: a clean gzip trailer is
		// exactly what made the empty archive look valid.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no stderr output"
		}
		return fmt.Errorf("pg_dump failed: %w: %s", waitErr, msg)
	}
	if copyErr != nil {
		return fmt.Errorf("stream pg_dump output: %w", copyErr)
	}
	if cerr := gz.Close(); cerr != nil {
		return fmt.Errorf("finish gzip: %w", cerr)
	}
	return nil
}

// dumpSQLite uses VACUUM INTO, which writes a consistent snapshot in one
// statement without blocking writers for the whole read.
//
// The temp file is removed on every path, including the error ones — a backup
// route that leaks a full copy of the database into a temp directory on each
// failure is its own problem.
func (d *Dumper) dumpSQLite(ctx context.Context, w io.Writer) error {
	dir, err := os.MkdirTemp("", "skein-backup-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// VACUUM INTO refuses to overwrite, so the path must not exist yet.
	target := filepath.Join(dir, "snapshot.db")

	sqlDB, err := sql.Open("sqlite", d.SQLitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if _, err := sqlDB.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}

	f, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(w)
	if _, cerr := io.Copy(gz, f); cerr != nil {
		return fmt.Errorf("stream snapshot: %w", cerr)
	}
	if cerr := gz.Close(); cerr != nil {
		return fmt.Errorf("finish gzip: %w", cerr)
	}
	return nil
}

// SchemaVersion reads the goose migration version.
func SchemaVersion(ctx context.Context, sqlDB *sql.DB) (int64, error) {
	var v int64
	err := sqlDB.QueryRowContext(ctx,
		"SELECT max(version_id) FROM goose_db_version").Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}
