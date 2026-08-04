package db_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mridul249/Skein/internal/db"
)

// THE REGRESSION TEST FOR 7b23f20, AS A PERMANENT GO TEST.
//
// The original bug: `pg_dump | gzip` under sh reports the exit status of the
// LAST command, so a pg_dump that never connected still produced a valid,
// EMPTY gzip archive and reported success. The archive was well-formed. You
// found out at restore time.
//
// This drives the Go implementation at a database that cannot exist. The
// requirements are both halves of what went wrong: an error must come back,
// AND no valid archive may reach the caller.
func TestDumpFailsLoudlyAgainstAMissingDatabase(t *testing.T) {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not installed")
	}

	d := db.NewDumper(db.DialectPostgres,
		"postgres://nobody@127.0.0.1:1/database_that_does_not_exist"+
			"?sslmode=disable&connect_timeout=2", "")

	var buf bytes.Buffer
	err := d.Dump(context.Background(), &buf)

	if err == nil {
		t.Fatal("Dump() = nil against a nonexistent database; " +
			"this is exactly the silent-success bug returning")
	}

	// The diagnosis has to survive. "exit status 1" alone is not actionable.
	if !strings.Contains(err.Error(), "pg_dump failed") {
		t.Errorf("error = %v, want it to name pg_dump", err)
	}

	// And critically: nothing that could be mistaken for a backup.
	if gzipIsValid(buf.Bytes()) {
		t.Fatalf("a VALID gzip archive (%d bytes) was produced by a failed dump; "+
			"a client would have saved this as a working backup", buf.Len())
	}
}

// The HTTP layer must not send a byte until the dump succeeds. Asserted at the
// unit level too: a failed dump leaves nothing decodable behind.
func TestFailedDumpProducesNoUsableArchive(t *testing.T) {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not installed")
	}
	d := db.NewDumper(db.DialectPostgres,
		"postgres://nobody@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2", "")

	var buf bytes.Buffer
	if err := d.Dump(context.Background(), &buf); err == nil {
		t.Fatal("Dump() = nil, want an error")
	}
	if _, err := gzip.NewReader(&buf); err == nil {
		t.Error("the failed dump left a readable gzip stream")
	}
}

// SQLite dumps go through VACUUM INTO, which is atomic and consistent.
func TestSQLiteDumpProducesARestorableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// goose_db_version must be present in the output: without it a restore
	// lands at an unknown migration state.
	mustExec(t, sqlDB, `CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY, version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL, tstamp TEXT NOT NULL)`)
	mustExec(t, sqlDB, `INSERT INTO goose_db_version (version_id, is_applied, tstamp)
		VALUES (4, 1, '2026-08-05T00:00:00Z')`)
	mustExec(t, sqlDB, `CREATE TABLE files (id TEXT PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, sqlDB, `INSERT INTO files VALUES ('f1', 'holiday.mkv')`)

	d := db.NewDumper(db.DialectSQLite, "", path)
	var buf bytes.Buffer
	if derr := d.Dump(context.Background(), &buf); derr != nil {
		t.Fatalf("Dump() = %v", derr)
	}
	if buf.Len() == 0 {
		t.Fatal("Dump() produced no output")
	}

	// Decompress and reopen: the snapshot has to be a working database, not
	// merely a well-formed archive.
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored.db")
	writeFile(t, restored, raw)

	rdb, err := sql.Open("sqlite", restored)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer func() { _ = rdb.Close() }()

	var name string
	if qerr := rdb.QueryRow(`SELECT name FROM files WHERE id='f1'`).Scan(&name); qerr != nil {
		t.Fatalf("query restored: %v", qerr)
	}
	if name != "holiday.mkv" {
		t.Errorf("restored name = %q, want holiday.mkv", name)
	}

	// Pre-serve verification: goose_db_version survived with its rows.
	var version int64
	if qerr := rdb.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&version); qerr != nil {
		t.Fatalf("goose_db_version missing from the snapshot: %v", qerr)
	}
	if version != 4 {
		t.Errorf("schema version = %d, want 4", version)
	}
}

// The temp snapshot must not survive the call, on success or failure. A backup
// route that leaves a full copy of the database in /tmp per invocation is its
// own disclosure problem.
func TestSQLiteDumpLeavesNoTempFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, _ := sql.Open("sqlite", path)
	mustExec(t, sqlDB, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	_ = sqlDB.Close()

	before := tempEntries(t)
	d := db.NewDumper(db.DialectSQLite, "", path)
	var buf bytes.Buffer
	if err := d.Dump(context.Background(), &buf); err != nil {
		t.Fatalf("Dump() = %v", err)
	}
	if after := tempEntries(t); after > before {
		t.Errorf("temp entries %d -> %d; the snapshot was not cleaned up", before, after)
	}

	// And the failure path, which is the one that actually tends to leak.
	bad := db.NewDumper(db.DialectSQLite, "", filepath.Join(t.TempDir(), "missing", "no.db"))
	beforeBad := tempEntries(t)
	var discard bytes.Buffer
	if err := bad.Dump(context.Background(), &discard); err == nil {
		t.Fatal("Dump() = nil against an unopenable database")
	}
	if after := tempEntries(t); after > beforeBad {
		t.Errorf("temp entries %d -> %d after a FAILED dump; cleanup missed the error path",
			beforeBad, after)
	}
}

// Concurrent invocations are rejected rather than stacked: two dumps of the
// same database at once turns a maintenance action into an outage.
func TestConcurrentDumpsAreRejectedNotStacked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, _ := sql.Open("sqlite", path)
	mustExec(t, sqlDB, `CREATE TABLE t (id INTEGER PRIMARY KEY)`)
	for i := 0; i < 2000; i++ {
		mustExec(t, sqlDB, `INSERT INTO t DEFAULT VALUES`)
	}
	_ = sqlDB.Close()

	d := db.NewDumper(db.DialectSQLite, "", path)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var busy, ok int
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			err := d.Dump(context.Background(), &buf)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, db.ErrBackupBusy):
				busy++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok == 0 {
		t.Error("every concurrent dump was refused; at least one must run")
	}
	if ok+busy != 6 {
		t.Errorf("ok=%d busy=%d, want them to total 6", ok, busy)
	}
}

// The dialect and schema version are both in the filename, so the Postgres v8
// / SQLite v4 difference is impossible to mistake for drift.
func TestFilenameNamesDialectAndVersion(t *testing.T) {
	when := time.Date(2026, 8, 5, 12, 30, 45, 0, time.UTC)

	pg := db.NewDumper(db.DialectPostgres, "postgres://x", "").Filename(8, when)
	if pg != "skein-backup-20260805-123045-postgres-v8.sql.gz" {
		t.Errorf("postgres filename = %q", pg)
	}
	lite := db.NewDumper(db.DialectSQLite, "", "/tmp/x.db").Filename(4, when)
	if lite != "skein-backup-20260805-123045-sqlite-v4.sql.gz" {
		t.Errorf("sqlite filename = %q", lite)
	}
}

func gzipIsValid(b []byte) bool {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	return err == nil
}

func mustExec(t *testing.T, d *sql.DB, q string) {
	t.Helper()
	if _, err := d.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tempEntries counts entries in the temp dir so a leaked snapshot is visible.
func tempEntries(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "skein-backup-") {
			n++
		}
	}
	return n
}
