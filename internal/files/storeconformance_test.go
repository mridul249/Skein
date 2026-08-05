package files

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

// Conformance harness for internal/files, built on the same design as
// internal/auth's and internal/accounts'.
//
// WHY: internal/files/sqlitestore.go is 574 lines that the desktop binary runs
// on, and until now nothing had ever executed them. Block 6 built the whole
// Go-side desktop download path on top of this store. Issue #33.
//
// The harness re-executes this package's own test binary with the backend
// flipped, so a test added to any *_test.go file extends the SQLite guarantee
// automatically. Two failure modes it is deliberately built to avoid: a
// hand-maintained list of "tests that also run on SQLite" (which rots), and a
// store selector that silently falls back to memory when the requested backend
// is unavailable (which reports success while testing nothing).
const filesBackendEnv = "SKEIN_FILES_TEST_BACKEND"

func filesBackend() string {
	if v := os.Getenv(filesBackendEnv); v != "" {
		return v
	}
	return "memory"
}

// ConformanceStore is Store plus the inspection helpers the existing tests
// reach for. Both MemoryStore and SQLiteStore must satisfy it.
//
// Exported because the suite lives in the external files_test package and
// needs to name this type in its fixtures. It is declared in a _test.go file,
// so it is not part of the shipped package's API.
type ConformanceStore interface {
	Store
	CorruptShard(fileID uuid.UUID, index int32, mutate func(*Shard))
	FileStatus(id uuid.UUID) (string, bool)
	ListShardsSnapshot() []Shard
	ShardCount(fileID uuid.UUID) int
}

// NewConformanceStore is the entry point for the external files_test package,
// where the existing suite lives. Exported from a _test.go file, so it exists
// only during tests and never in the shipped binary.
func NewConformanceStore(t testing.TB) ConformanceStore {
	t.Helper()
	return newConformanceStore(t)
}

// newConformanceStore builds the store for the active backend. An unknown
// value is a hard failure, never a silent fallback: a conformance suite that
// quietly runs the default backend twice reports confidence it has not earned.
func newConformanceStore(t testing.TB) ConformanceStore {
	t.Helper()
	switch b := filesBackend(); b {
	case "memory":
		return NewMemoryStore()
	case "sqlite":
		return newTestFilesSQLiteStore(t)
	default:
		t.Fatalf("unknown %s=%q", filesBackendEnv, b)
		return nil
	}
}

// newTestFilesSQLiteStore opens a scratch database and applies the files
// migrations.
//
// A file rather than :memory: — an in-memory SQLite database is
// per-connection and database/sql pools connections, so a second pooled
// connection would see an empty schema and fail with a baffling "no such
// table" partway through a test.
func newTestFilesSQLiteStore(t testing.TB) *SQLiteStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "files.db")
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applyFilesSQLiteMigrations(t, db)
	seedFilesReferences(t, db)
	return NewSQLiteStore(db)
}

// seedFilesReferences satisfies the foreign keys the files tables carry.
//
// files.user_id and folders.user_id reference users; file_shards
// .connected_account_id references connected_accounts. foreign_keys is ON, so
// a test that invents ids would fail on the constraint. MemoryStore has no
// such requirement — exactly the kind of difference this run exists to
// surface — but it is a property of the FIXTURE, not of the code under test,
// so the fixture absorbs it rather than the tests being weakened.
func seedFilesReferences(t testing.TB, db *sql.DB) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY) STRICT`,
		`CREATE TABLE IF NOT EXISTS connected_accounts (id TEXT PRIMARY KEY) STRICT`,
		// Mirror whatever ids the tests invent, so the references resolve
		// without every test knowing about this fixture.
		`CREATE TRIGGER IF NOT EXISTS seed_user_folders
		 BEFORE INSERT ON folders
		 BEGIN INSERT OR IGNORE INTO users (id) VALUES (NEW.user_id); END`,
		`CREATE TRIGGER IF NOT EXISTS seed_user_files
		 BEFORE INSERT ON files
		 BEGIN INSERT OR IGNORE INTO users (id) VALUES (NEW.user_id); END`,
		`CREATE TRIGGER IF NOT EXISTS seed_account_shards
		 BEFORE INSERT ON file_shards
		 WHEN NEW.connected_account_id IS NOT NULL
		 BEGIN INSERT OR IGNORE INTO connected_accounts (id) VALUES (NEW.connected_account_id); END`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("seed references: %v", err)
		}
	}
}

// applyFilesSQLiteMigrations runs the "-- +goose Up" half of the files
// migration. goose itself is not used: it wants its own driver registration
// and a version table these tests do not care about.
func applyFilesSQLiteMigrations(t testing.TB, db *sql.DB) {
	t.Helper()

	dir := filepath.Join(filesRepoRoot(t), "internal", "db", "migrations", "sqlite")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sqlite migrations: %v", err)
	}
	applied := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") || !strings.Contains(name, "files") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		up := filesUpSection(string(raw))
		if strings.TrimSpace(up) == "" {
			t.Fatalf("%s has no +goose Up section", name)
		}
		if _, eerr := db.Exec(up); eerr != nil {
			t.Fatalf("apply %s: %v", name, eerr)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("no files migrations found in %s", dir)
	}
}

func filesUpSection(s string) string {
	_, after, found := strings.Cut(s, "-- +goose Up")
	if !found {
		return ""
	}
	before, _, _ := strings.Cut(after, "-- +goose Down")
	return before
}

func filesRepoRoot(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestFilesSQLiteConformance re-runs this package's entire suite against
// SQLiteStore.
func TestFilesSQLiteConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("re-runs the whole package against SQLite")
	}
	if filesBackend() != "memory" {
		t.Skip("already inside a conformance child run")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	cmd := exec.Command(exe, "-test.v", "-test.count", "1")
	cmd.Env = append(os.Environ(), filesBackendEnv+"=sqlite")
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Errorf("the files suite does not pass against SQLiteStore: %v", err)
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
