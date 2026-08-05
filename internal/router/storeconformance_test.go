package router

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Conformance harness for internal/router, same design as auth's, accounts'
// and files'. Issue #33.
//
// internal/router/sqlitestore.go is 190 lines the desktop binary runs on, and
// nothing had ever executed them. The reservation path is where capacity is
// claimed atomically, so a divergence here is not cosmetic: it is the
// difference between a drive being oversubscribed and not.
const routerBackendEnv = "SKEIN_ROUTER_TEST_BACKEND"

func routerBackend() string {
	if v := os.Getenv(routerBackendEnv); v != "" {
		return v
	}
	return "memory"
}

// conformanceStore is Store plus the inspection helpers the tests assert on.
type conformanceStore interface {
	Store
	AddAccount(id uuid.UUID, ordinal int32, email string, total, used int64)
	ReservedOn(accountID uuid.UUID) int64
	Expire(uploadID uuid.UUID)
}

// newConformanceStore builds the store for the active backend. An unknown
// value is a hard failure, never a silent fallback to memory.
func newConformanceStore(t testing.TB) conformanceStore {
	t.Helper()
	switch b := routerBackend(); b {
	case "memory":
		return NewMemoryStore()
	case "sqlite":
		return newTestRouterSQLiteStore(t)
	default:
		t.Fatalf("unknown %s=%q", routerBackendEnv, b)
		return nil
	}
}

// testRouterSQLiteStore is SQLiteStore plus the inspection helpers. They live
// here rather than on the production type because AddAccount and Expire write
// states no production caller should be able to create.
type testRouterSQLiteStore struct {
	*SQLiteStore
	db     *sql.DB
	userID uuid.UUID
}

func newTestRouterSQLiteStore(t testing.TB) *testRouterSQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "router.db")
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applyRouterSQLiteMigrations(t, db)
	return &testRouterSQLiteStore{SQLiteStore: NewSQLiteStore(db), db: db, userID: testRouterUser}
}

// Candidates ignores the user filter, exactly as MemoryStore does.
//
// MemoryStore has no notion of ownership: AddAccount registers an account and
// Candidates returns every one. The real query filters on ca.user_id, and the
// tests generate a fresh random userID per test while AddAccount takes none —
// so a faithful fixture returns nothing and every planner test reads "No drive
// is connected".
//
// THE FIRST ATTEMPT REWROTE user_id TO MATCH THE CALLER, AND THAT WAS WRONG:
// concurrent planners with different user ids then fought over the same rows,
// which -race caught as an intermittent "No drive is connected". A fixture
// must not introduce a shared mutable that the code under test does not have.
// Reading without the filter is both correct and stateless.
//
// This is a property of the FIXTURE, not of the code under test: these tests
// are about placement arithmetic, not ownership. Ownership is covered where it
// belongs, in the files package's multi-user isolation tests, which run
// against a store that does filter.
func (s *testRouterSQLiteStore) Candidates(ctx context.Context, _ uuid.UUID) ([]Candidate, error) {
	return s.SQLiteStore.Candidates(ctx, s.userID)
}

// testRouterUser owns fixture accounts until a caller asks under its own id.
var testRouterUser = uuid.MustParse("11111111-1111-1111-1111-111111111111")

// AddAccount inserts the connected_accounts and storage_accounts rows that
// Candidates joins.
func (s *testRouterSQLiteStore) AddAccount(id uuid.UUID, ordinal int32, email string, total, used int64) {
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO users (id) VALUES (?)`, s.userID.String()); err != nil {
		panic("AddAccount user: " + err.Error())
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO connected_accounts (id, user_id, kind, provider_account_id, email,
                                ordinal, status, access_token_enc, created_at, updated_at)
VALUES (?, ?, 'gdrive', ?, ?, ?, 'active', X'00', ?, ?)`,
		id.String(), s.userID.String(), id.String(), email, ordinal, now, now); err != nil {
		panic("AddAccount connected_accounts: " + err.Error())
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO storage_accounts (connected_account_id, total_bytes, used_bytes,
                              reserved_bytes, last_synced_at)
VALUES (?, ?, ?, 0, ?)`, id.String(), total, used, now); err != nil {
		panic("AddAccount storage_accounts: " + err.Error())
	}
}

// ReservedOn reports the reserved byte counter for an account.
func (s *testRouterSQLiteStore) ReservedOn(accountID uuid.UUID) int64 {
	var n int64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT reserved_bytes FROM storage_accounts WHERE connected_account_id = ?`,
		accountID.String()).Scan(&n); err != nil {
		return 0
	}
	return n
}

// Expire backdates an upload's reservations so the reclaim janitor sees them.
func (s *testRouterSQLiteStore) Expire(uploadID uuid.UUID) {
	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE quota_reservations SET expires_at = ? WHERE upload_id = ?`,
		past, uploadID.String()); err != nil {
		panic("Expire: " + err.Error())
	}
}

// applyRouterSQLiteMigrations applies the migrations the router tables need:
// accounts (connected_accounts, storage_accounts), files (referenced by
// uploads), and reservations itself.
func applyRouterSQLiteMigrations(t testing.TB, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY) STRICT`); err != nil {
		t.Fatalf("create users stand-in: %v", err)
	}

	dir := filepath.Join(routerRepoRoot(t), "internal", "db", "migrations", "sqlite")
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
		// 00001 is auth's; the users stand-in above covers the one reference
		// that matters and keeps this fixture independent of auth's schema.
		if strings.Contains(name, "auth") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		up := routerUpSection(string(raw))
		if strings.TrimSpace(up) == "" {
			t.Fatalf("%s has no +goose Up section", name)
		}
		if _, eerr := db.Exec(up); eerr != nil {
			t.Fatalf("apply %s: %v", name, eerr)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("no migrations found in %s", dir)
	}
}

func routerUpSection(s string) string {
	_, after, found := strings.Cut(s, "-- +goose Up")
	if !found {
		return ""
	}
	before, _, _ := strings.Cut(after, "-- +goose Down")
	return before
}

func routerRepoRoot(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestRouterSQLiteConformance re-runs this package's entire suite against
// SQLiteStore.
func TestRouterSQLiteConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("re-runs the whole package against SQLite")
	}
	if routerBackend() != "memory" {
		t.Skip("already inside a conformance child run")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	cmd := exec.Command(exe, "-test.v", "-test.count", "1")
	cmd.Env = append(os.Environ(), routerBackendEnv+"=sqlite")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Errorf("the router suite does not pass against SQLiteStore: %v", err)
		t.Logf("child run output:\n%s", out)
		return
	}
	if !strings.Contains(string(out), "PASS") {
		t.Errorf("child run produced no PASS; output:\n%s", out)
	}
	if strings.Count(string(out), "--- PASS") < 5 {
		t.Errorf("child run executed suspiciously few tests; output:\n%s", out)
	}
}
