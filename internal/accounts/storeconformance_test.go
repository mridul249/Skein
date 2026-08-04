package accounts

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// TODO(#32): Finish migrating service_test.go to the backend-agnostic
// conformance harness. Temporarily excluded from the `unused` linter.

// Same design as internal/auth/storeconformance_test.go, and for the same
// reason: the exit bar is the existing suite passing unmodified against both
// backends, not a parallel set of SQLite-only tests that can drift.
//
// The harness re-executes this package's own test binary with the backend
// flipped, so a test added to service_test.go extends the SQLite guarantee
// automatically. Two failure modes it is deliberately built to avoid:
// a hand-maintained list of "tests that also run on SQLite" (which rots), and
// a store selector that silently falls back to the default when the requested
// backend is unavailable (which reports success while testing nothing -- the
// auth port had exactly that bug, via a test that hardcoded NewMemoryStore).

const backendEnv = "SKEIN_ACCOUNTS_TEST_BACKEND"

func activeBackend() string {
	if v := os.Getenv(backendEnv); v != "" {
		return v
	}
	return "memory"
}

// conformanceStore is Store plus the inspection helpers the tests assert on.
// MemoryStore and SQLiteStore both satisfy it.
type conformanceStore interface {
	Store
	PendingStateCount() int
	HasStateHash(hash []byte) bool
	PendingVerifiers() []string
	SetReserved(accountID uuid.UUID, reserved int64)
}

// newConformanceStore builds the store for the current backend. An unknown
// value is a hard failure rather than a silent fallback to memory.
func newConformanceStore(t *testing.T) conformanceStore {
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
// migrations.
//
// A file rather than :memory: -- an in-memory SQLite database is
// per-connection and database/sql pools connections, so a second pooled
// connection would see an empty schema and fail with a baffling "no such
// table" partway through a test.
func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "accounts.db")
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applySQLiteMigrations(t, db)
	seedConformanceUser(t, db)
	return NewSQLiteStore(db)
}

// seedConformanceUser inserts the users rows the accounts tables reference.
//
// connected_accounts.user_id and oauth_states.user_id are foreign keys into
// users, and foreign_keys is ON, so a test that invents a user id would fail
// on the constraint. MemoryStore has no such requirement, which is exactly the
// kind of difference this conformance run exists to surface -- but it is a
// property of the fixture, not of the code under test, so the fixture absorbs
// it rather than the tests being weakened.
//
// The tests generate random user ids, so rather than guess them, foreign key
// enforcement for the users reference is relaxed to a trigger-free insert of
// any id on demand. Simpler: create the users table permissively and let the
// store insert what it likes.
func seedConformanceUser(t *testing.T, db *sql.DB) {
	t.Helper()
	// The auth migration owns the real users table; here only the referenced
	// key matters. A permissive stand-in keeps this package's fixture
	// independent of auth's schema.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY) STRICT`); err != nil {
		t.Fatalf("create users stand-in: %v", err)
	}
	// Accept any user id the tests invent: a trigger mirrors inserts into
	// users so the foreign keys resolve without the tests knowing about it.
	for _, ddl := range []string{
		`CREATE TRIGGER IF NOT EXISTS seed_user_ca
		 BEFORE INSERT ON connected_accounts
		 BEGIN INSERT OR IGNORE INTO users (id) VALUES (NEW.user_id); END`,
		`CREATE TRIGGER IF NOT EXISTS seed_user_os
		 BEFORE INSERT ON oauth_states
		 BEGIN INSERT OR IGNORE INTO users (id) VALUES (NEW.user_id); END`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create seed trigger: %v", err)
		}
	}
}

// applySQLiteMigrations runs the "-- +goose Up" half of every SQLite migration
// that this package's tables need.
//
// goose itself is not used: it would need its own driver registration and a
// version table these tests do not care about. The migrations are plain DDL,
// so splitting on the markers is enough.
func applySQLiteMigrations(t *testing.T, db *sql.DB) {
	t.Helper()

	dir := filepath.Join(repoRootForTest(t), "internal", "db", "migrations", "sqlite")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sqlite migrations: %v", err)
	}
	applied := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		// 00001 is auth's; this package only needs the accounts tables, and
		// applying auth's would drag in its users/sessions schema. The users
		// stand-in above covers the one reference that matters.
		if !strings.Contains(name, "accounts") {
			continue
		}
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
		applied++
	}
	if applied == 0 {
		t.Fatalf("no accounts migrations found in %s", dir)
	}
}

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
// SQLiteStore. Skipped under -short and inside a child run.
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

	cmd := exec.Command(exe, "-test.v", "-test.count", "1")
	cmd.Env = append(os.Environ(), backendEnv+"=sqlite")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Errorf("the accounts suite does not pass against SQLiteStore: %v", err)
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
