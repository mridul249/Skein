package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// openMigrationDB opens a scratch SQLite database with the same DSN pragmas
// OpenSQLite uses in production. foreign_keys(1) in particular is not
// incidental here: it is the pragma that makes the 00005 rebuild dangerous,
// so a test that dropped it would be testing a configuration the desktop
// binary never runs.
func openMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	dbh, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	goose.SetLogger(goose.NopLogger())
	return dbh
}

func mustExec(t *testing.T, d *sql.DB, query string) {
	t.Helper()
	if _, err := d.Exec(query); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

// seedOneFileWithAShard writes the minimum row set that makes the rebuild's
// blast radius observable: a file with a child shard row.
func seedOneFileWithAShard(t *testing.T, dbh *sql.DB, status string) {
	t.Helper()
	mustExec(t, dbh, `INSERT INTO users (id, email, password_hash, created_at, updated_at)
	                  VALUES ('u1', 'a@b.c', 'h', 't', 't')`)
	mustExec(t, dbh, `INSERT INTO files (id, user_id, name, size_bytes, status, created_at, updated_at)
	                  VALUES ('f1', 'u1', 'n.bin', 10, '`+status+`', 't', 't')`)
	mustExec(t, dbh, `INSERT INTO file_shards (id, file_id, idx, provider_object_id,
	                      size_bytes, plain_size_bytes, plain_offset, created_at)
	                  VALUES ('s1', 'f1', 0, 'p1', 10, 10, 0, 't')`)
}

// The 00005 bundle rebuilds `files`, because SQLite has no DROP CONSTRAINT and
// widening files_status_check is otherwise inexpressible. A rebuild drops the
// table, and file_shards references files(id) ON DELETE CASCADE — so with
// foreign keys enforced, the rebuild deletes the shard mapping of every file
// in the database while reporting success.
//
// The guard is PRAGMA foreign_keys=off around the rebuild, which only takes
// effect OUTSIDE a transaction, which is why the migration is marked
// `-- +goose NO TRANSACTION`.
//
// MUTATION-VERIFIED: removing that one line takes file_shards from 1 to 0 here.
// Note that the constraint and index assertions below stay GREEN under that
// mutation — the shard count is the only thing that catches it, which is why
// this test seeds a child row rather than asserting on schema shape alone.
func TestSQLiteBundleRebuildPreservesShardsAndConstraints(t *testing.T) {
	dbh := openMigrationDB(t)
	ctx := context.Background()

	// Stop before the bundle so the data it must preserve already exists.
	// Migrating fully and inserting afterwards would test a rebuild that had
	// nothing to lose.
	if err := goose.UpToContext(ctx, dbh, "migrations/sqlite", 4); err != nil {
		t.Fatalf("migrate to v4: %v", err)
	}
	seedOneFileWithAShard(t, dbh, "ready")

	if err := goose.UpContext(ctx, dbh, "migrations/sqlite"); err != nil {
		t.Fatalf("apply bundle: %v", err)
	}

	var shards int
	if err := dbh.QueryRow(`SELECT count(*) FROM file_shards`).Scan(&shards); err != nil {
		t.Fatalf("count shards: %v", err)
	}
	if shards != 1 {
		t.Errorf("file_shards = %d, want 1: the rebuild cascade-deleted the shard mapping", shards)
	}

	var files int
	if err := dbh.QueryRow(`SELECT count(*) FROM files`).Scan(&files); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if files != 1 {
		t.Errorf("files = %d, want 1", files)
	}

	// The widened states are now storable.
	for _, status := range []string{"partially_missing", "corrupted"} {
		if _, err := dbh.Exec(`UPDATE files SET status = ? WHERE id = 'f1'`, status); err != nil {
			t.Errorf("status %q rejected after widening: %v", status, err)
		}
	}

	// And the constraint still bites. A rebuild that silently dropped
	// files_status_check would accept the two values above and be
	// indistinguishable from a correct one without this.
	if _, err := dbh.Exec(`UPDATE files SET status = 'junk' WHERE id = 'f1'`); err == nil {
		t.Error("a garbage status was accepted: the rebuild dropped files_status_check")
	}

	// Indexes live on the table, so the rebuild drops them too.
	var indexes int
	if err := dbh.QueryRow(`SELECT count(*) FROM sqlite_master
	                         WHERE type = 'index' AND tbl_name = 'files'
	                           AND name LIKE 'files_%'`).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexes != 3 {
		t.Errorf("files indexes = %d, want 3: the rebuild did not restore them", indexes)
	}

	var violations int
	if err := dbh.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Errorf("foreign_key_check reported %d violations", violations)
	}
}

// Down is a second rebuild with the same hazard, and it additionally has to
// deal with rows whose status the narrow constraint cannot hold.
func TestSQLiteBundleDownUpRoundTrip(t *testing.T) {
	dbh := openMigrationDB(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, dbh, "migrations/sqlite"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	seedOneFileWithAShard(t, dbh, "corrupted")

	if err := goose.DownContext(ctx, dbh, "migrations/sqlite"); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	// A status the narrow constraint cannot express is folded back rather than
	// failing the migration. The damage is in the drives, not in this column.
	var status string
	if err := dbh.QueryRow(`SELECT status FROM files WHERE id = 'f1'`).Scan(&status); err != nil {
		t.Fatalf("read status after down: %v", err)
	}
	if status != "ready" {
		t.Errorf("status after down = %q, want %q", status, "ready")
	}

	assertShardCount(t, dbh, 1, "down")

	if err := goose.UpContext(ctx, dbh, "migrations/sqlite"); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	assertShardCount(t, dbh, 1, "re-up")
}

func assertShardCount(t *testing.T, dbh *sql.DB, want int, stage string) {
	t.Helper()
	var got int
	if err := dbh.QueryRow(`SELECT count(*) FROM file_shards`).Scan(&got); err != nil {
		t.Fatalf("count shards after %s: %v", stage, err)
	}
	if got != want {
		t.Errorf("file_shards = %d after %s, want %d: the rebuild destroyed the shard mapping", got, stage, want)
	}
}
