package auth

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The exit bar for Phase 7 Task 3: the same auth suite passes unmodified
// against both backends. Not "the SQLite store compiles" and not "a parallel
// set of SQLite tests passes" -- the *same* assertions, so a behavioural
// difference between the two stores fails a test rather than going unnoticed
// until the desktop build misbehaves in a way Postgres never did.
//
// How it works: newTestService (service_test.go) builds whichever store
// activeBackend names. TestSQLiteConformance re-executes this package's own
// test binary with that variable flipped, so every test in the package runs a
// second time against SQLite. Adding a test to service_test.go therefore
// extends the SQLite guarantee automatically -- there is no second list to
// keep in sync, which is the failure mode a hand-maintained conformance list
// always eventually hits.

// backendEnv selects the store under test in a child run.
const backendEnv = "SKEIN_AUTH_TEST_BACKEND"

// activeBackend is "memory" (the default) or "sqlite".
func activeBackend() string {
	if v := os.Getenv(backendEnv); v != "" {
		return v
	}
	return "memory"
}

// newConformanceStore builds the store for the current backend. Every test in
// this package reaches it through newTestService.
func newConformanceStore(t *testing.T) testStore {
	t.Helper()
	switch b := activeBackend(); b {
	case "memory":
		return NewMemoryStore()
	case "sqlite":
		return newTestSQLiteStore(t)
	default:
		t.Fatalf("unknown %s=%q", backendEnv, b)
		return nil
	}
}

// newTestSQLiteStore opens a scratch database and applies the SQLite
// migrations to it.
//
// The database is a real file rather than :memory:. An in-memory SQLite
// database is per-connection, and database/sql pools connections, so a second
// pooled connection would see an empty schema -- the failure shows up as a
// baffling "no such table" partway through a test. A file in t.TempDir() is
// shared correctly across the pool and removed when the test ends.
func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite() = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applySQLiteMigrations(t, db)
	return NewSQLiteStore(db)
}

// applySQLiteMigrations runs the goose "-- +goose Up" half of every migration
// in internal/db/migrations/sqlite.
//
// goose itself is not used: it would need its own driver registration and a
// version table this test does not care about. The migrations are plain DDL
// with no templating, so splitting on the Up/Down markers is enough and keeps
// the test dependency-free.
func applySQLiteMigrations(t *testing.T, db *sql.DB) {
	t.Helper()

	dir := filepath.Join(repoRootForTest(t), "internal", "db", "migrations", "sqlite")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sqlite migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no sqlite migrations found in %s", dir)
	}
	// ReadDir sorts by filename, and the migrations are numbered, so this is
	// already migration order.
	for _, name := range names {
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		up := upSection(string(raw))
		if strings.TrimSpace(up) == "" {
			t.Fatalf("%s has no +goose Up section", name)
		}
		if _, eerr := db.Exec(up); eerr != nil {
			t.Fatalf("apply %s: %v", name, eerr)
		}
	}
}

// upSection returns the SQL between "-- +goose Up" and "-- +goose Down".
func upSection(s string) string {
	_, after, found := strings.Cut(s, "-- +goose Up")
	if !found {
		return ""
	}
	before, _, _ := strings.Cut(after, "-- +goose Down")
	return before
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestSQLiteConformance re-runs this package's entire test suite against
// SQLiteStore.
//
// It shells out to the compiled test binary rather than looping in-process
// because Go's testing package cannot re-enter itself, and because a child
// process gives each backend a clean start: no shared globals, no ordering
// coupling between the two runs.
//
// Skipped under -short (it doubles the package's test time) and inside a child
// run (or it would recurse).
func TestSQLiteConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("re-runs the whole package against SQLite")
	}
	if activeBackend() != "memory" {
		t.Skip("already inside a conformance child run")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	// The child runs every test in the package. This one skips itself there
	// (the activeBackend guard above), so there is no recursion and no
	// exclusion pattern to keep correct as tests are added.
	cmd := exec.Command(exe, "-test.v", "-test.count", "1")
	cmd.Env = append(os.Environ(), backendEnv+"=sqlite")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Errorf("the auth suite does not pass against SQLiteStore: %v", err)
		t.Logf("child run output:\n%s", out)
		return
	}
	// A run that executed nothing would "pass" vacuously.
	if !strings.Contains(string(out), "PASS") {
		t.Errorf("child run produced no PASS; output:\n%s", out)
	}
	if strings.Count(string(out), "--- PASS") < 5 {
		t.Errorf("child run executed suspiciously few tests; output:\n%s", out)
	}
}
